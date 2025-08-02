/*
Copyright 2025 The KubeLB Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package kubelb

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"

	kubelbv1alpha1 "k8c.io/kubelb/api/ce/kubelb.k8c.io/v1alpha1"
	"k8c.io/kubelb/internal/tunnel"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	TunnelControllerName = "tunnel-controller"
)

// TunnelReconciler reconciles a Tunnel Object
type TunnelReconciler struct {
	ctrlruntimeclient.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	Namespace          string
	EnvoyProxyTopology EnvoyProxyTopology
	DisableGatewayAPI  bool
}

// +kubebuilder:rbac:groups=kubelb.k8c.io,resources=tunnels,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kubelb.k8c.io,resources=tunnels/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kubelb.k8c.io,resources=tunnels/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

func (r *TunnelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("name", req.NamespacedName)

	log.Info("Reconciling")

	tunnel := &kubelbv1alpha1.Tunnel{}
	if err := r.Get(ctx, req.NamespacedName, tunnel); err != nil {
		if kerrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// Before proceeding further we need to make sure that the resource is reconcilable.
	tenant, config, err := GetTenantAndConfig(ctx, r.Client, r.Namespace, RemoveTenantPrefix(tunnel.Namespace))
	if err != nil {
		log.Error(err, "unable to fetch Tenant and Config, cannot proceed")
		return reconcile.Result{}, err
	}

	// Check if tunneling is disabled in the config
	if config.Spec.Tunnel.Disable {
		log.Info("Tunneling is disabled in config, skipping reconciliation")
		if err := r.updateStatus(ctx, tunnel, kubelbv1alpha1.TunnelPhaseFailed, "Tunneling is disabled in configuration"); err != nil {
			log.Error(err, "failed to update status")
		}
		return reconcile.Result{}, nil
	}

	// Check if connection manager URL is configured
	if config.Spec.Tunnel.ConnectionManagerURL == "" {
		log.Error(nil, "Connection manager URL not configured")
		if err := r.updateStatus(ctx, tunnel, kubelbv1alpha1.TunnelPhaseFailed, "Connection manager URL not configured"); err != nil {
			log.Error(err, "failed to update status")
		}
		return reconcile.Result{}, fmt.Errorf("connection manager URL not configured in Config resource")
	}

	// Resource is marked for deletion
	if tunnel.DeletionTimestamp != nil {
		if controllerutil.ContainsFinalizer(tunnel, CleanupFinalizer) {
			return r.cleanup(ctx, log, tunnel)
		}
		// Finalizer doesn't exist so clean up is already done
		return reconcile.Result{}, nil
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(tunnel, CleanupFinalizer) {
		if ok := controllerutil.AddFinalizer(tunnel, CleanupFinalizer); !ok {
			log.Error(nil, "Failed to add finalizer for the Tunnel")
			return ctrl.Result{Requeue: true}, nil
		}
		if err := r.Update(ctx, tunnel); err != nil {
			return reconcile.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
	}

	err = r.reconcile(ctx, log, tunnel, config, tenant)
	if err != nil {
		log.Error(err, "reconciling failed")
		if statusErr := r.updateStatus(ctx, tunnel, kubelbv1alpha1.TunnelPhaseFailed, err.Error()); statusErr != nil {
			log.Error(statusErr, "failed to update status")
		}
		r.Recorder.Eventf(tunnel, corev1.EventTypeWarning, "ReconcileFailed", "Failed to reconcile tunnel: %v", err)
	}

	return reconcile.Result{}, err
}

func (r *TunnelReconciler) reconcile(ctx context.Context, log logr.Logger, tunnelObj *kubelbv1alpha1.Tunnel, config *kubelbv1alpha1.Config, tenant *kubelbv1alpha1.Tenant) error {
	log.V(1).Info("Starting reconciliation")

	// Update status to pending if not already set
	if tunnelObj.Status.Phase == "" {
		if err := r.updateStatus(ctx, tunnelObj, kubelbv1alpha1.TunnelPhasePending, "Provisioning tunnel"); err != nil {
			return fmt.Errorf("failed to update status: %w", err)
		}
	}

	// Get annotations from tenant and config
	annotations := GetAnnotations(tenant, config)

	// Reconcile the tunnel using the business logic
	tunnelReconciler := tunnel.NewReconciler(r.Client, r.Scheme, r.Recorder, r.DisableGatewayAPI)
	hostname, routeRef, err := tunnelReconciler.Reconcile(ctx, log, tunnelObj, config, tenant, annotations)
	if err != nil {
		return fmt.Errorf("failed to reconcile tunnel: %w", err)
	}

	// Update status with the results
	tunnelObj.Status.Hostname = hostname
	tunnelObj.Status.URL = fmt.Sprintf("https://%s", hostname)
	tunnelObj.Status.ConnectionManagerURL = config.Spec.Tunnel.ConnectionManagerURL
	tunnelObj.Status.Resources.RouteRef = routeRef
	tunnelObj.Status.Resources.ServiceName = fmt.Sprintf("tunnel-envoy-%s", tunnelObj.Namespace)
	tunnelObj.Status.Resources.ServerTLSSecretName = fmt.Sprintf("%s-server-tls", tunnelObj.Name)
	tunnelObj.Status.Resources.ClientTLSSecretName = fmt.Sprintf("%s-client-tls", tunnelObj.Name)

	if err := r.updateStatus(ctx, tunnelObj, kubelbv1alpha1.TunnelPhaseReady, "Tunnel is ready"); err != nil {
		return fmt.Errorf("failed to update status: %w", err)
	}

	r.Recorder.Eventf(tunnelObj, corev1.EventTypeNormal, "TunnelReady", "Tunnel is ready at %s", tunnelObj.Status.URL)

	return nil
}

func (r *TunnelReconciler) cleanup(ctx context.Context, log logr.Logger, tunnelObj *kubelbv1alpha1.Tunnel) (ctrl.Result, error) {
	log.Info("Cleaning up tunnel")

	// Update status
	if err := r.updateStatus(ctx, tunnelObj, kubelbv1alpha1.TunnelPhaseTerminating, "Cleaning up tunnel"); err != nil {
		log.Error(err, "failed to update status during cleanup")
	}

	// Clean up using business logic
	tunnelReconciler := tunnel.NewReconciler(r.Client, r.Scheme, r.Recorder, r.DisableGatewayAPI)
	if err := tunnelReconciler.Cleanup(ctx, tunnelObj); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to cleanup tunnel: %w", err)
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(tunnelObj, CleanupFinalizer)
	if err := r.Update(ctx, tunnelObj); err != nil {
		return reconcile.Result{}, fmt.Errorf("failed to remove finalizer: %w", err)
	}

	r.Recorder.Eventf(tunnelObj, corev1.EventTypeNormal, "TunnelDeleted", "Tunnel has been deleted")

	return reconcile.Result{}, nil
}

func (r *TunnelReconciler) updateStatus(ctx context.Context, tunnel *kubelbv1alpha1.Tunnel, phase kubelbv1alpha1.TunnelPhase, message string) error {
	tunnel.Status.Phase = phase

	// Update conditions
	condition := metav1.Condition{
		Type:               string(phase),
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             string(phase),
		Message:            message,
	}

	// Replace or add the condition
	found := false
	for i, c := range tunnel.Status.Conditions {
		if c.Type == condition.Type {
			tunnel.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		tunnel.Status.Conditions = append(tunnel.Status.Conditions, condition)
	}

	return r.Status().Update(ctx, tunnel)
}

// SetupWithManager sets up the controller with the Manager.
func (r *TunnelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kubelbv1alpha1.Tunnel{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

# kubelb-manager

Helm chart for KubeLB Manager. This is used to deploy the KubeLB CCM to a Kubernetes cluster. The CCM is responsible for propagating the load balancer configurations to the management cluster.

![Version: v1.1.0](https://img.shields.io/badge/Version-v1.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v1.1.0](https://img.shields.io/badge/AppVersion-v1.1.0-informational?style=flat-square)

## Installing the chart

### Pre-requisites

* Create a namespace `kubelb` for the CCM to be deployed in.

### Install helm chart

Now, we can install the helm chart:

```sh
helm pull oci://quay.io/kubermatic/helm-charts/kubelb-manager --version=v1.1.0 --untardir "kubelb-manager" --untar
## Create and update values.yaml with the required values.
helm install kubelb-manager kubelb-manager --namespace kubelb -f values.yaml --create-namespace
```

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` |  |
| autoscaling.enabled | bool | `false` |  |
| autoscaling.maxReplicas | int | `10` |  |
| autoscaling.minReplicas | int | `1` |  |
| autoscaling.targetCPUUtilizationPercentage | int | `80` |  |
| autoscaling.targetMemoryUtilizationPercentage | int | `80` |  |
| fullnameOverride | string | `""` |  |
| image.pullPolicy | string | `"IfNotPresent"` |  |
| image.repository | string | `"quay.io/kubermatic/kubelb-manager"` |  |
| image.tag | string | `"v1.1.0"` |  |
| imagePullSecrets | list | `[]` |  |
| kkpintegration.rbac | bool | `false` | Create RBAC for KKP integration. |
| kubelb.debug | bool | `true` |  |
| kubelb.enableGatewayAPI | bool | `false` | enableGatewayAPI specifies whether to enable the Gateway API and Gateway Controllers. By default Gateway API is disabled since without Gateway APIs installed the controller cannot start. |
| kubelb.enableLeaderElection | bool | `true` |  |
| kubelb.enableTenantMigration | bool | `true` |  |
| kubelb.envoyProxy.affinity | object | `{}` |  |
| kubelb.envoyProxy.nodeSelector | object | `{}` |  |
| kubelb.envoyProxy.replicas | int | `3` | The number of replicas for the Envoy Proxy deployment. |
| kubelb.envoyProxy.resources | object | `{}` |  |
| kubelb.envoyProxy.singlePodPerNode | bool | `true` | Deploy single pod per node. |
| kubelb.envoyProxy.tolerations | list | `[]` |  |
| kubelb.envoyProxy.topology | string | `"shared"` | Topology defines the deployment topology for Envoy Proxy. Valid values are: shared and global. |
| kubelb.envoyProxy.useDaemonset | bool | `false` | Use DaemonSet for Envoy Proxy deployment instead of Deployment. |
| kubelb.propagateAllAnnotations | bool | `false` | Propagate all annotations from the LB resource to the LB service. |
| kubelb.propagatedAnnotations | object | `{}` | Allowed annotations that will be propagated from the LB resource to the LB service. |
| kubelb.skipConfigGeneration | bool | `false` | Set to true to skip the generation of the Config CR. Useful when the config CR needs to be managed manually. |
| kubelb.tunnel | object | `{"connectionManager":{"affinity":{},"grpcAddr":":9090","healthCheck":{"enabled":true,"livenessInitialDelay":30,"readinessInitialDelay":10},"httpAddr":":8080","image":{"pullPolicy":"IfNotPresent","repository":"quay.io/kubermatic/kubelb-connection-manager","tag":"latest"},"ingress":{"annotations":{},"className":"","enabled":false,"hosts":[{"host":"tunnel.example.com","paths":[{"path":"/tunnel.TunnelService","pathType":"Prefix"}]}],"tls":[]},"nodeSelector":{},"podAnnotations":{},"podLabels":{},"podSecurityContext":{"fsGroup":65534,"runAsNonRoot":true,"runAsUser":65534},"replicaCount":1,"requestTimeout":"30s","resources":{"limits":{"cpu":"200m","memory":"128Mi"},"requests":{"cpu":"50m","memory":"64Mi"}},"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsNonRoot":true,"runAsUser":65534},"service":{"grpcPort":9090,"httpPort":8080,"type":"ClusterIP"},"tolerations":[]},"enabled":false}` | Tunnel configuration |
| kubelb.tunnel.connectionManager.grpcAddr | string | `":9090"` | Server addresses |
| kubelb.tunnel.connectionManager.healthCheck | object | `{"enabled":true,"livenessInitialDelay":30,"readinessInitialDelay":10}` | Health check configuration |
| kubelb.tunnel.connectionManager.image | object | `{"pullPolicy":"IfNotPresent","repository":"quay.io/kubermatic/kubelb-connection-manager","tag":"latest"}` | Connection manager image configuration |
| kubelb.tunnel.connectionManager.ingress | object | `{"annotations":{},"className":"","enabled":false,"hosts":[{"host":"tunnel.example.com","paths":[{"path":"/tunnel.TunnelService","pathType":"Prefix"}]}],"tls":[]}` | Ingress configuration for external gRPC access |
| kubelb.tunnel.connectionManager.podAnnotations | object | `{}` | Pod configuration |
| kubelb.tunnel.connectionManager.replicaCount | int | `1` | Number of connection manager replicas |
| kubelb.tunnel.connectionManager.resources | object | `{"limits":{"cpu":"200m","memory":"128Mi"},"requests":{"cpu":"50m","memory":"64Mi"}}` | Resource limits |
| kubelb.tunnel.connectionManager.securityContext | object | `{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"readOnlyRootFilesystem":true,"runAsNonRoot":true,"runAsUser":65534}` | Security context |
| kubelb.tunnel.connectionManager.service | object | `{"grpcPort":9090,"httpPort":8080,"type":"ClusterIP"}` | Service configuration |
| kubelb.tunnel.enabled | bool | `false` | Enable tunnel functionality |
| nameOverride | string | `""` |  |
| nodeSelector | object | `{}` |  |
| podAnnotations | object | `{}` |  |
| podLabels | object | `{}` |  |
| podSecurityContext.runAsNonRoot | bool | `true` |  |
| podSecurityContext.seccompProfile.type | string | `"RuntimeDefault"` |  |
| rbac.allowLeaderElectionRole | bool | `true` |  |
| rbac.allowMetricsReaderRole | bool | `true` |  |
| rbac.allowProxyRole | bool | `true` |  |
| rbac.enabled | bool | `true` |  |
| replicaCount | int | `1` |  |
| resources.limits.cpu | string | `"500m"` |  |
| resources.limits.memory | string | `"512Mi"` |  |
| resources.requests.cpu | string | `"100m"` |  |
| resources.requests.memory | string | `"128Mi"` |  |
| securityContext.allowPrivilegeEscalation | bool | `false` |  |
| securityContext.capabilities.drop[0] | string | `"ALL"` |  |
| securityContext.runAsUser | int | `65532` |  |
| service.port | int | `8001` |  |
| service.protocol | string | `"TCP"` |  |
| service.type | string | `"ClusterIP"` |  |
| serviceAccount.annotations | object | `{}` |  |
| serviceAccount.create | bool | `true` |  |
| serviceAccount.name | string | `""` |  |
| serviceMonitor.enabled | bool | `false` |  |
| tolerations | list | `[]` |  |

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| Kubermatic | <support@kubermatic.com> | <https://kubermatic.com> |
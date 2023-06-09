# Terraform Operator Remote Controller

This is another controller that listens to tf resource events. Add and update events will transmit the tf manifest to a TFO-API server.

## Usage

Set up the following environment vars for the API server:

```bash
CLIENT_NAME=foo-k8s-cluster
TFO_API_PROTOCOL=http
TFO_API_HOST=localhost
TFO_API_PORT=5001
TFO_API_LOGIN_USER=username
TFO_API_LOGIN_PASSWORD=password
```

Make sure you're connected to a cluster:

```bash
KUBECONFIG=~/.kube/config
```

> InCluster configuration is automatically configured when this binary runs in a kubernetes pod.

### RBAC

This will require `get` and `list` permissions to `terraforms` resources:


```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
 name: tforc
rules:
- apiGroups:
  - tf.galleybytes.com
  resources:
  - terraforms
  verbs:
  - get
  - list
```

And run the binary or run an in-cluster container:

```bash
go run main.go
```

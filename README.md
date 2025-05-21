# Terraform Operator Remote Controller

This is another controller that listens to tf resource events. Add and update events will transmit the tf manifest to a TFO-API server.

## Usage

Set up the following environment vars for the API server:

```bash
CLIENT_NAME=foo-k8s-cluster
I3_API_PROTOCOL=http
I3_API_HOST=localhost
I3_API_PORT=5001
I3_API_LOGIN_USER=username
I3_API_LOGIN_PASSWORD=password
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

Additional rules can be added when defining a post job to run after a successful terraform workflow.

```yaml

# ADDITIONAL RULES
- apiGroups:
  - batch
  resources:
  - jobs
  verbs:
  - '*'
- apiGroups:
  - ""
  resources:
  - serviceaccounts
  verbs:
  - create
  - get
  - list
- apiGroups:
  - rbac.authorization.k8s.io
  resources:
  - clusterrolebindings
  verbs:
  - create
  - get
  - list
- apiGroups:
  - ""
  resources:
  - configmaps
  verbs:
  - '*'
  ```

And run the binary or run an in-cluster container:

```bash
go run main.go
```

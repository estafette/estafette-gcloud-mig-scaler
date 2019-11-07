# estafette-gcloud-mig-scaler
This applications sets the minimum number of instances for a Google Cloud managed instance group based on request volume retrieved from Prometheus.

## Installation

Prepare using Helm:

```
brew install kubernetes-helm
kubectl -n kube-system create serviceaccount tiller
kubectl create clusterrolebinding tiller --clusterrole=cluster-admin --serviceaccount=kube-system:tiller
helm init --service-account tiller --wait
```

Then install or upgrade with Helm:

```
helm repo add estafette https://helm.estafette.io
helm upgrade --install estafette-gcloud-mig-scaler --namespace estafette estafette/estafette-gcloud-mig-scaler
```
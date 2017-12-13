FROM scratch

LABEL maintainer="estafette.io" \
      description="The estafette-gcloud-mig-scaler assists autoscaling of a managed instance group based on request rate"

COPY ca-certificates.crt /etc/ssl/certs/
COPY estafette-gcloud-mig-scaler /

ENTRYPOINT ["/estafette-gcloud-mig-scaler"]

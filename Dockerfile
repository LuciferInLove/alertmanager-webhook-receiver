FROM scratch
COPY alertmanager-webhook-receiver  /
CMD ["/alertmanager-webhook-receiver"]

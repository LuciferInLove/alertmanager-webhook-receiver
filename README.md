[![Go Report Card](https://goreportcard.com/badge/github.com/LuciferInLove/alertmanager-webhook-receiver)](https://goreportcard.com/report/github.com/LuciferInLove/alertmanager-webhook-receiver)
![Build status](https://github.com/LuciferInLove/alertmanager-webhook-receiver/workflows/Build/badge.svg)

# alertmanager-webhook-receiver

Receives [Prometheus Alertmanager](https://prometheus.io/docs/alerting/alertmanager/) webhook messages in the format described in the [webhook receiver docs](https://prometheus.io/docs/alerting/latest/configuration/#webhook_config). It's just a fork of the [prometheus-webhook-receiver](https://gitlab.cern.ch/paas-tools/monitoring/prometheus-webhook-receiver) originally released by [Daniel Juarez of cern.ch](mailto:daniel.juarez.gonzalez@cern.ch). This fork supports go templates as described below.

## Overview

**alertmanager-webhook-receiver** receives messages in POST bodies to `/alerts` in JSON, either when an alert is fired or when it's resolved, and creates Kubernetes Jobs that are described in specified configMap. If the alert annotations contain a YAML Job definition corresponding to the event (firing/resolved), the Job
will be created in the receiver's local namespace. This Job can be used to automatically repair the problem that caused the alert.
**alertmanager-webhook-receiver** supports go templates in Job descriptions. 

## Usage

```
Usage of alertmanager-webhook-receiver:
  -configmap-namespace string
        Kubernetes namespace where jobs are defined (default "namespace_where_alertmanager_webhook_receiver_located")
  -job-destination-namespace string
        Kubernetes namespace where jobs will be created (default "namespace_where_alertmanager_webhook_receiver_located")
  -listen-address string
        Address to listen for webhook (default ":9270")
  -log-level string
        Only log messages with the given severity or above. One of: [debug, info, warn, error] (default "info")
  -responses-configmap string
        Configmap containing YAML job definitions that support Go templates (default "receiver-job-definitions")
```

**alertmanager-webhook-receiver** reads namespace from `/var/run/secrets/kubernetes.io/serviceaccount/namespace` if namespaces aren't defined in command line flags. It also reads [Kubernetes Service Account Token](https://kubernetes.io/docs/reference/access-authn-authz/authentication/#service-account-tokens) from `/var/run/secrets/kubernetes.io/serviceaccount/token`.

## How it works

* First of all, it is necessary to create Prometheus alert rule(s) with special annotations - `firing_job` and `resolved_job` as described below in Alert definitions.
* Prometheus notifies the Alertmanager whenever an alert meets the rule conditions.
* Alertmanager decides to which webhook receiver it sends the alert depending on its [routes](https://prometheus.io/docs/alerting/configuration/#%3Croute%3E)
* Alertmanager sends the notification to the configured receivers, i.e. email and webhook receivers. See an example of Alertmanager configuration for **alertmanager-webhook-receiver** below.
* **alertmanager-webhook-receiver** gets the JSON definition of the alert including the annotations to retrieve the corresponding `firing_job` or `resolved_job` YAML job definition from the configMap named by default as `receiver-job-definitions`.
* **alertmanager-webhook-receiver** creates a job following this previous definition by default on the current Kubernetes namespace where the receiver is running.
* If the Job definition contains go templates, **alertmanager-webhook-receiver** will resolve them using labels from alert as values.

## Alert definitions

Alert definitions are located in [Prometheus Alerting Rules](https://prometheus.io/docs/prometheus/latest/configuration/alerting_rules/).

In order to respond to a Prometheus alert, the alert definition needs to have the following annotations:

`firing_job`: This annotation corresponds to the key on the `receiver-job-definitions` configMap with the job response to run when the Prometheus rule fires an alert.
`resolved_job`: This annotation corresponds to the key on the `receiver-job-definitions` configMap with the job response to run when the Prometheus rule sends the resolution to an alert.

This is an example alarm:
```bash

  - alert: gitlab_elasticsearch_cluster_not_healthy
    expr: elasticsearch_cluster_health_status{kubernetes_namespace="gitlab",color="red"} == 1 OR absent(elasticsearch_cluster_health_status{kubernetes_namespace="gitlab"})
    for: 10m
    labels:
      severity: critical
      job: elasticsearch
    annotations:
      firing_job: disable_global_search
      resolved_job: enable_global_search
```

## ConfigMap with jobs definitions

Job definitions should be stored as YAML on the configMap named by default as `receiver-job-definitions`. 

Example:

```bash
apiVersion: v1
kind: ConfigMap
metadata:
  name: receiver-job-definitions
data:
  disable_global_search: |
    apiVersion: batch/v1
    kind: Job
    metadata:
      generateName: firing-gitlab-elasticsearch-cluster-not-healthy-
      labels:
        app: alertmanager-webhook-receiver
    spec:
      ttlSecondsAfterFinished: 100
      parallelism: 1
      completions: 1
      template:
        metadata:
          labels:
            app: alertmanager-webhook-receiver
        spec:
          restartPolicy: Never
          containers:
          - ...
  enable_global_search: |
    apiVersion: batch/v1
    kind: Job
    metadata:
      generateName: resolved-gitlab-elasticsearch-cluster-not-healthy-
      labels:
        app: alertmanager-webhook-receiver
    spec:
      ttlSecondsAfterFinished: 100
      parallelism: 1
      completions: 1
      template:
        metadata:
          labels:
            app: alertmanager-webhook-receiver
        spec:
          restartPolicy: Never
          containers:
           - ...
```

### Go templates in job definitions

Job definitions can contain go templates to resolve lables from alert as values. Templates must be specified as `{{ .Values.label_name }}.

For example:

```bash
apiVersion: v1
kind: ConfigMap
metadata:
  name: receiver-job-definitions
data:
  test_firing_job: |
    apiVersion: batch/v1
    kind: Job
    metadata:
      generateName: firing-test-
      labels:
        app: alertmanager-webhook-receiver
    spec:
      ttlSecondsAfterFinished: 100
      parallelism: 1
      completions: 1
      template:
        metadata:
          labels:
            app: alertmanager-webhook-receiver
        spec:
          restartPolicy: Never
          containers:
          - name: bash
            image: "bash"
            imagePullPolicy: Always
            args:
            - echo
            - "{{ .Values.job }}"
```

If you use configMap as a part of helm templates, you can escape **alertmanager-webhook-receiver** templates as:

```
{{`{{ .Values.job }}`}}
```

## Alertmanager configuration example

```yaml
config:
  receivers:
  - name: elk
    webhook_configs:
    - url: http://alertmanager-webhook-receiver-service-name-in-kubernetes.alertmanager-webhook-receiver-namespace.svc.cluster.local:9270/alerts
  route:
    routes:
    - receiver: elk
      match_re:
        label_node_kubernetes_io_nodename: elk
        alertname: NodeCPUUsage|NodeCPULoadIsTooHigh|NodeMemoryUsage|NodeDiskIsFull
```

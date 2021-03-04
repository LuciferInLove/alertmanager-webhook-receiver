package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"net/http"
	"os"

	"github.com/sirupsen/logrus"
	log "github.com/sirupsen/logrus"

	"github.com/ghodss/yaml"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type (

	// HookMessage is the message we receive from Alertmanager
	HookMessage struct {
		Version           string            `json:"version"`
		GroupKey          string            `json:"groupKey"`
		TruncatedAlerts   uint64            `json:"truncatedAlerts"`
		Status            string            `json:"status"`
		Receiver          string            `json:"receiver"`
		GroupLabels       map[string]string `json:"groupLabels"`
		CommonLabels      map[string]string `json:"commonLabels"`
		CommonAnnotations map[string]string `json:"commonAnnotations"`
		ExternalURL       string            `json:"externalURL"`
		Alerts            []Alert           `json:"alerts"`
	}

	// Alert holds one alert for notification templates
	Alert struct {
		Status       string            `json:"status"`
		Labels       map[string]string `json:"labels"`
		Annotations  map[string]string `json:"annotations"`
		StartsAt     string            `json:"startsAt,omitempty"`
		EndsAt       string            `json:"EndsAt,omitempty"`
		GeneratorURL string            `json:"generatorURL"`
		Fingerprint  string            `json:"fingerprint"`
	}

	// Kubernetes clients for groups.
	k8sClientSet struct {
		clientset               kubernetes.Clientset
		jobDestinationNamespace string
		configmapNamespace      string
		responsesConfigmap      string
	}
)

var (
	// Allowed log levels
	logLevelsList [4]string = [4]string{"debug", "info", "warn", "error"}
)

func main() {
	log.SetFormatter(&log.TextFormatter{
		DisableColors: true,
		DisableQuote:  true,
		FullTimestamp: true,
	})

	log.Infof("Starting webhook receiver")

	// Extract the current namespace from the mounted secrets
	defaultK8sNamespaceLocation := "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
	if _, err := os.Stat(defaultK8sNamespaceLocation); os.IsNotExist(err) {
		log.Errorf("Current kubernetes namespace could not be found")
		log.Fatalf(err.Error())
	}

	namespaceFileData, err := ioutil.ReadFile(defaultK8sNamespaceLocation)
	currentNamespace := string(namespaceFileData)

	// Namespace where job configmap is located. If ommited, current namespace will be used
	configmapNamespace := flag.String("configmap-namespace", currentNamespace, "Kubernetes namespace where jobs are defined")

	// Namespace where jobs will be created. If ommited, current namespace will be used
	jobDestinationNamespace := flag.String("job-destination-namespace", currentNamespace, "Kubernetes namespace where jobs will be created")

	// ConfigMap where job definitions are stored
	responsesConfigmap := flag.String("responses-configmap", "receiver-job-definitions", "Configmap containing YAML job definitions that support Go templates")

	listenAddress := flag.String("listen-address", ":9270", "Address to listen for webhook")
	logLevel := flag.String("log-level", "info", "Only log messages with the given severity or above. One of: [debug, info, warn, error]")

	flag.Parse()

	logrusLogLevel, err := LogLevelContains(logLevelsList, *logLevel)
	if err != nil {
		log.Errorf(err.Error())
		flag.Usage()
		os.Exit(1)
	}

	log.SetLevel(logrusLogLevel)

	// Create k8s in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf(err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf(err.Error())
	}

	c := &k8sClientSet{
		clientset:               *clientset,
		jobDestinationNamespace: *jobDestinationNamespace,
		responsesConfigmap:      *responsesConfigmap,
		configmapNamespace:      *configmapNamespace,
	}

	http.HandleFunc("/healthz", healthzHandler)
	http.HandleFunc("/alerts", c.alertsHandler)
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}

// LogLevelContains looks for defined log level in the logLevelsList
func LogLevelContains(slice [4]string, value string) (logrus.Level, error) {
	for _, item := range slice {
		if item == value {
			logrusLogLevel, err := log.ParseLevel(value)
			return logrusLogLevel, err
		}
	}
	return 0, fmt.Errorf("There was a wrong log level defined: %v", value)
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	io.WriteString(w, "Webhook receiver is running\n")
}

func (c *k8sClientSet) alertsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		c.postHandler(w, r)
	case http.MethodGet:
		c.getHandler(w)
	default:
		http.Error(w, "Unsupported HTTP method: "+string(r.Method), 400)
	}
}

func (c *k8sClientSet) postHandler(w http.ResponseWriter, r *http.Request) {

	jsonDecoder := json.NewDecoder(r.Body)
	ctx := r.Context()
	defer r.Body.Close()

	var message HookMessage
	if err := jsonDecoder.Decode(&message); err != nil {
		log.Errorf("Error during decoding message: %v", err)
		http.Error(w, "Invalid request body", 400)
		return
	}

	status := message.Status
	alertName := message.CommonLabels["alertname"]

	log.Infof("Alert received: " + alertName + "[" + status + "]")

	for k, v := range message.CommonLabels {
		log.Debugf("Label: %s = %s", k, v)
	}
	for k, v := range message.CommonAnnotations {
		log.Debugf("Annotation: %s = %s", k, v)
	}

	// Extract the key to look for on the responses configMap
	firingJobKey := message.CommonAnnotations["firing_job"]
	resolvedJobKey := message.CommonAnnotations["resolved_job"]

	// Decide wheter it is a firing alarm or a resolved one, depending on the used annotation
	if len(firingJobKey) > 0 && status == "firing" {
		c.createResponseJob(ctx, message.CommonLabels, firingJobKey, w)
	} else if len(resolvedJobKey) > 0 && status == "resolved" {
		c.createResponseJob(ctx, message.CommonLabels, resolvedJobKey, w)
	} else {
		log.Warnf("Received alarm without correct response configuration, ommiting reponses")
		return
	}
}

func (c *k8sClientSet) getHandler(w http.ResponseWriter) {
	// Alertmanager expects an 200 OK response, otherwise send_resolved will never work
	enc := json.NewEncoder(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	if err := enc.Encode("OK"); err != nil {
		log.Errorf("Error during encoding messages: %v", err)
	}
}

func (c *k8sClientSet) createResponseJob(ctx context.Context, commonLabels map[string]string, jobKey string, w http.ResponseWriter) {
	log.Debugf("Retrieving configMap...")

	configMap, err := c.clientset.CoreV1().ConfigMaps(c.configmapNamespace).Get(ctx, c.responsesConfigmap, metav1.GetOptions{})
	if err != nil {
		log.Errorf("Error while retrieving configMap %v: %v", c.responsesConfigmap, err)
		http.Error(w, "Webhook error during retrieving configMap with job definitions", 500)
		return
	}

	log.Debugf("ConfigMap is retrieved")

	jobDefinition := configMap.Data[jobKey]

	// Values created from alert labels
	valuesFromLabels := map[string]interface{}{"Values": commonLabels}

	// Parse job definition configMap to insert values from labels
	tmpl := template.Must(template.New("").Parse(jobDefinition))
	var buffer bytes.Buffer
	if err := tmpl.Execute(&buffer, valuesFromLabels); err != nil {
		log.Errorf(err.Error())
	}

	yamlJobDefinition := []byte(buffer.Bytes())

	// yamlJobDefinition contains a []byte of the yaml job spec
	// Convert the yaml to json so it works with Unmarshal
	jsonBytes, err := yaml.YAMLToJSON(yamlJobDefinition)
	if err != nil {
		log.Errorf("Error while converting YAML job definition to JSON: %v", err)
		http.Error(w, "Webhook error during creating a job", 500)
		return
	}

	jobObject := &batchv1.Job{}
	err = json.Unmarshal(jsonBytes, jobObject)
	if err != nil {
		log.Errorf("Error while using unmarshal on received job: %v", err)
		http.Error(w, "Webhook error creating a job", 500)
		return
	}

	// Job client for creating the job according to the job definitions extracted from the responses configMap
	jobsClient := c.clientset.BatchV1().Jobs(c.jobDestinationNamespace)

	// Create job
	log.Infof("Creating job...")
	result, err := jobsClient.Create(ctx, jobObject, metav1.CreateOptions{})
	if err != nil {
		log.Errorf("Error while creating job: %v", err)
		http.Error(w, "Webhook error during creating a job", 500)
		return
	}

	log.Infof("Created job")

	prettyResult, err := json.MarshalIndent(result, "", "    ")
	if err == nil {
		log.Debugln(string(prettyResult))
	}
}

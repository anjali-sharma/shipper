package webhook

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"mime"
	"net/http"
	"reflect"
	"time"

	admission "k8s.io/api/admission/v1beta1"
	kubeclient "k8s.io/api/admission/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"

	shipper "github.com/bookingcom/shipper/pkg/apis/shipper/v1alpha1"
	clientset "github.com/bookingcom/shipper/pkg/client/clientset/versioned"
	informers "github.com/bookingcom/shipper/pkg/client/informers/externalversions"
	listers "github.com/bookingcom/shipper/pkg/client/listers/shipper/v1alpha1"
	"github.com/bookingcom/shipper/pkg/metrics/prometheus"
	"github.com/bookingcom/shipper/pkg/util/rolloutblock"
)

const (
	AgentName = "webhook"
)

type Webhook struct {
	shipperClientset    clientset.Interface
	rolloutBlocksLister listers.RolloutBlockLister
	rolloutBlocksSynced cache.InformerSynced

	bindAddr string
	bindPort string

	tlsCertFile       string
	tlsPrivateKeyFile string

	webhookHealthMetric prometheus.WebhookMetric
	heartbeatPeriod     time.Duration
}

var (
	runtimeScheme = runtime.NewScheme()
	codecs        = serializer.NewCodecFactory(runtimeScheme)
	deserializer  = codecs.UniversalDeserializer()
)

func NewWebhook(
	bindAddr, bindPort, tlsPrivateKeyFile, tlsCertFile string,
	shipperClientset clientset.Interface,
	shipperInformerFactory informers.SharedInformerFactory,
	webhookMetric prometheus.WebhookMetric,
	heartbeatPeriod time.Duration,
) *Webhook {
	rolloutBlocksInformer := shipperInformerFactory.Shipper().V1alpha1().RolloutBlocks()

	return &Webhook{
		shipperClientset:    shipperClientset,
		rolloutBlocksLister: rolloutBlocksInformer.Lister(),
		rolloutBlocksSynced: rolloutBlocksInformer.Informer().HasSynced,

		bindAddr: bindAddr,
		bindPort: bindPort,

		tlsPrivateKeyFile: tlsPrivateKeyFile,
		tlsCertFile:       tlsCertFile,

		webhookHealthMetric: webhookMetric,
		heartbeatPeriod:     heartbeatPeriod,
	}
}

func (c *Webhook) Run(stopCh <-chan struct{}) {
	addr := c.bindAddr + ":" + c.bindPort
	mux := c.initializeHandlers()
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	if !cache.WaitForCacheSync(stopCh, c.rolloutBlocksSynced) {
		klog.Fatalf("failed to wait for caches to sync")
		return
	}

	ctx, cancelHeartbeat := context.WithCancel(context.Background())
	c.startHeartbeatRoutine(ctx, addr)

	go func() {
		var serverError error
		if c.tlsCertFile == "" || c.tlsPrivateKeyFile == "" {
			serverError = server.ListenAndServe()
		} else {
			c.observeCertificateExpiration(addr)

			serverError = server.ListenAndServeTLS(c.tlsCertFile, c.tlsPrivateKeyFile)
		}

		if serverError != nil && serverError != http.ErrServerClosed {
			klog.Fatalf("failed to start shipper-webhook: %v", serverError)
			cancelHeartbeat()
		}
	}()

	klog.V(2).Info("Started the WebHook")

	<-stopCh

	klog.V(2).Info("Shutting down the WebHook")

	if err := server.Shutdown(context.Background()); err != nil {
		klog.Errorf(`HTTP server Shutdown: %v`, err)
	}
}

func (c *Webhook) observeCertificateExpiration(addr string) {
	cert, err := tls.LoadX509KeyPair(c.tlsCertFile, c.tlsPrivateKeyFile)
	if err != nil {
		klog.Errorf("fail to load TLS certificate from file with private key %v", err)
		return
	}
	certificate, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		klog.Errorf("fail to parse TLS certificate %v", err)
		return
	}
	expiryTime := certificate.NotAfter
	c.webhookHealthMetric.ObserveCertificateExpiration(addr, expiryTime)
	klog.V(8).Infof("Shipper Validating Webhooks TLS certificate expires on %v", certificate.NotAfter)
}

func (c *Webhook) startHeartbeatRoutine(ctx context.Context, host string) {
	ticker := time.NewTicker(c.heartbeatPeriod)
	go func() {
		for {
			select {
			case <-ticker.C:
				c.webhookHealthMetric.ObserveHeartBeat(host)
			case <-ctx.Done():
				ticker.Stop()
				return
			}
		}
	}()
}

func (c *Webhook) initializeHandlers() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/validate", adaptHandler(c.validateHandlerFunc))
	return mux
}

// adaptHandler wraps an admission review function to be consumed through HTTP.
func adaptHandler(handler func(*admission.AdmissionReview) *admission.AdmissionResponse) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil {
			if data, err := ioutil.ReadAll(r.Body); err == nil {
				body = data
			}
		}

		if len(body) == 0 {
			http.Error(w, "empty body", http.StatusBadRequest)
			return
		}

		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			http.Error(w, "Invalid content-type", http.StatusUnsupportedMediaType)
			return
		}

		if mediaType != "application/json" {
			http.Error(w, "invalid Content-Type, expect `application/json`", http.StatusUnsupportedMediaType)
			return
		}

		var admissionResponse *admission.AdmissionResponse
		ar := admission.AdmissionReview{}
		if _, _, err := deserializer.Decode(body, nil, &ar); err != nil {
			admissionResponse = &admission.AdmissionResponse{
				Result: &metav1.Status{
					Message: err.Error(),
				},
			}
		} else {
			admissionResponse = handler(&ar)
		}

		admissionReview := admission.AdmissionReview{}
		if admissionResponse != nil {
			admissionReview.Response = admissionResponse
			if ar.Request != nil {
				admissionReview.Response.UID = ar.Request.UID
			}
		}

		resp, err := json.Marshal(admissionReview)
		if err != nil {
			http.Error(w, fmt.Sprintf("could not encode response: %v", err), http.StatusInternalServerError)
			return
		}

		if _, err := w.Write(resp); err != nil {
			http.Error(w, fmt.Sprintf("could not write response: %v", err), http.StatusInternalServerError)
			return
		}
	}
}

func (c *Webhook) validateHandlerFunc(review *admission.AdmissionReview) *admission.AdmissionResponse {
	request := review.Request
	var err error

	switch request.Kind.Kind {
	case "Application":
		var application shipper.Application
		err = json.Unmarshal(request.Object.Raw, &application)
		if err == nil {
			err = c.validateApplication(request, application)
		}
	case "Release":
		var release shipper.Release
		err = json.Unmarshal(request.Object.Raw, &release)
		if err == nil {
			err = c.validateRelease(request, release)
		}
	case "Cluster":
		var cluster shipper.Cluster
		err = json.Unmarshal(request.Object.Raw, &cluster)
	case "InstallationTarget":
		var installationTarget shipper.InstallationTarget
		err = json.Unmarshal(request.Object.Raw, &installationTarget)
	case "CapacityTarget":
		var capacityTarget shipper.CapacityTarget
		err = json.Unmarshal(request.Object.Raw, &capacityTarget)
	case "TrafficTarget":
		var trafficTarget shipper.TrafficTarget
		err = json.Unmarshal(request.Object.Raw, &trafficTarget)
	case "RolloutBlock":
		var rolloutBlock shipper.RolloutBlock
		err = json.Unmarshal(request.Object.Raw, &rolloutBlock)
	}

	if err != nil {
		return &admission.AdmissionResponse{
			Result: &metav1.Status{
				Message: err.Error(),
			},
		}
	}

	return &admission.AdmissionResponse{
		Allowed: true,
	}
}

func (c *Webhook) validateRelease(request *admission.AdmissionRequest, release shipper.Release) error {
	var err error
	overrides, existingBlocks, err := rolloutblock.GetAllBlocks(c.rolloutBlocksLister, &release)
	if err != nil {
		return err
	}
	if err = rolloutblock.ValidateAnnotations(existingBlocks, overrides); err != nil {
		return err
	}
	switch request.Operation {
	case kubeclient.Create:
		err = rolloutblock.ValidateBlocks(existingBlocks, overrides)
	case kubeclient.Update:
		var oldRelease shipper.Release
		err = json.Unmarshal(request.OldObject.Raw, &oldRelease)
		if err != nil {
			return err
		}

		// validate against rollout blocks
		if !reflect.DeepEqual(release.Spec, oldRelease.Spec) {
			err = rolloutblock.ValidateBlocks(existingBlocks, overrides)
		}

		// make sure the environment wasn't changed
		if !reflect.DeepEqual(release.Spec.Environment, oldRelease.Spec.Environment) {
			return fmt.Errorf("the Release environment must not be changed; consider editing the Application object")
		}
	}

	return err
}

func (c *Webhook) validateApplication(request *admission.AdmissionRequest, application shipper.Application) error {
	var err error
	overrides, existingBlocks, err := rolloutblock.GetAllBlocks(c.rolloutBlocksLister, &application)
	if err != nil {
		return err
	}
	if err = rolloutblock.ValidateAnnotations(existingBlocks, overrides); err != nil {
		return err
	}
	switch request.Operation {
	case kubeclient.Create:
		err = rolloutblock.ValidateBlocks(existingBlocks, overrides)
	case kubeclient.Update:
		var oldApp shipper.Application
		err = json.Unmarshal(request.OldObject.Raw, &oldApp)
		if err != nil {
			return err
		}

		if !reflect.DeepEqual(application.Spec, oldApp.Spec) {
			err = rolloutblock.ValidateBlocks(existingBlocks, overrides)
		}
	}

	return err
}

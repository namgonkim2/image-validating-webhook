package server

import (
	"crypto/tls"
	"encoding/json"
	"io/ioutil"
	"net/http"

	"k8s.io/api/admission/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

var (
	scheme = runtime.NewScheme()
	codecs = serializer.NewCodecFactory(scheme)
)

// AdmissionController is ...
type AdmissionController interface {
	HandleAdmission(review *v1beta1.AdmissionReview) error
}

// AdmissionControllerServer is ...
type AdmissionControllerServer struct {
	AdmissionController AdmissionController
	Decoder             runtime.Decoder
}

func (admissionControllerServer *AdmissionControllerServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, err := ioutil.ReadAll(request.Body)

	if err != nil {
		// logging error
	}

	review := &v1beta1.AdmissionReview{}
	_, _, err = admissionControllerServer.Decoder.Decode(body, nil, review)

	if err != nil {
		// logging error
	}

	admissionControllerServer.AdmissionController.HandleAdmission(review)
	responseInBytes, err := json.Marshal(review)

	if _, err := writer.Write(responseInBytes); err != nil {
		// logging error
	}
}

func getAdmissionServerWithoutSSL(admissionController AdmissionController, listenOn string) *http.Server {
	server := &http.Server{
		Handler: &AdmissionControllerServer{
			AdmissionController: admissionController,
			Decoder:             codecs.UniversalDeserializer(),
		},
		Addr: listenOn,
	}

	return server
}

// GetAdmissionValidationServer is ...
func GetAdmissionValidationServer(admissionController AdmissionController, tlsCert, tlsKey, listenOn string) *http.Server {
	serverCert, err := tls.LoadX509KeyPair(tlsCert, tlsKey)
	server := getAdmissionServerWithoutSSL(admissionController, listenOn)
	server.TLSConfig = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
	}

	if err != nil {
		// logging error
	}

	return server
}

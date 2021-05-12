package server

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/tmax-cloud/image-validating-webhook/internal/k8s"
	whv1 "github.com/tmax-cloud/image-validating-webhook/pkg/type"
	regv1 "github.com/tmax-cloud/registry-operator/api/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// DockerHandler handles overall process to check signs
type DockerHandler struct {
	client         *kubernetes.Clientset
	whiteList      WhiteList
	pod            *corev1.Pod
	patch          *corev1.Pod
	dindPodName    string
	signerPolicies []whv1.SignerPolicy
}

// ImageInfo stores an image's info
type ImageInfo struct {
	registry string
	name     string
	tag      string
}

// ExecResult is a result of cli command
type ExecResult struct {
	OutBuffer *bytes.Buffer
	ErrBuffer *bytes.Buffer
}

func newDockerHandler(pod *corev1.Pod) (*DockerHandler, error) {
	kubeCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)
	restCfg, _ := kubeCfg.ClientConfig()
	clientset, _ := kubernetes.NewForConfig(restCfg)

	// Read whitelist
	wl, err := ReadWhiteList(clientset)
	if err != nil {
		return nil, err
	}

	pods, _ := clientset.CoreV1().Pods(dindNamespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", dindDeployment),
	})
	dindPod := corev1.Pod{}
	if len(pods.Items) > 0 {
		dindPod = pods.Items[0]
	}

	signerPolicies := &whv1.SignerPolicyList{}
	if err := clientset.RESTClient().
		Get().AbsPath("apis/tmax.io/v1").
		Resource("signerpolicies").
		Namespace(pod.Namespace).
		Do(context.TODO()).
		Into(signerPolicies); err != nil {
		return nil, fmt.Errorf("signer policies error, %s", err)
	}

	return &DockerHandler{
		client:         clientset,
		pod:            pod,
		patch:          pod.DeepCopy(),
		whiteList:      *wl,
		dindPodName:    dindPod.GetName(),
		signerPolicies: signerPolicies.Items,
	}, nil
}

// GetPatch generates a patch to update pod spec
func (h *DockerHandler) GetPatch() *corev1.Pod {
	return h.patch
}

func getDigest(image string, signatures []Signature) string {
	digest := ""
	for _, signedTag := range signatures[0].SignedTags {
		if signedTag.SignedTag == getImageInfo(image).tag {
			digest = signedTag.Digest
		}
	}

	return digest
}

func (h *DockerHandler) isValid() (bool, string) {
	isValid, name := h.addDigestWhenImageValid(true)

	if isValid {
		isValid, name = h.addDigestWhenImageValid(false)
	}

	return isValid, name
}

func (h *DockerHandler) addDigestWhenImageValid(isInitContainer bool) (bool, string) {
	isValid := true
	name := ""
	containers := h.pod.Spec.InitContainers
	if !isInitContainer {
		containers = h.pod.Spec.Containers
	}

	for i, container := range containers {
		if !h.isImageInWhiteList(container.Image) {
			validation, digest := h.isSignedImage(container.Image)
			isValid = isValid && validation

			if !isValid {
				name = container.Image
				break
			} else {
				if isInitContainer {
					h.patch.Spec.InitContainers[i].Image = fmt.Sprintf("%s@sha256:%s", container.Image, digest)
				} else {
					h.patch.Spec.Containers[i].Image = fmt.Sprintf("%s@sha256:%s", container.Image, digest)
				}
			}
		}
	}

	return isValid, name
}

func (h *DockerHandler) makeCommand(imageInfo ImageInfo) string {
	notaryServer := h.findNotaryServer(imageInfo.registry)
	var command string
	if notaryServer == "docker.io" {
		command = fmt.Sprintf("unset DOCKER_CONTENT_TRUST_SERVER; docker trust inspect %s:%s", imageInfo.name, imageInfo.tag)
	} else if notaryServer == "docker-hub" {
		command = fmt.Sprintf("unset DOCKER_CONTENT_TRUST_SERVER; docker trust inspect %s/%s:%s", imageInfo.registry, imageInfo.name, imageInfo.tag)
	} else {
		if err := h.loginToRegistry(imageInfo.registry); err != nil {
			log.Printf("Couldn't login to registry named %s: by %s", imageInfo.registry, err)
		}
		command = fmt.Sprintf("export DOCKER_CONTENT_TRUST_SERVER=%s; docker trust inspect %s/%s:%s", notaryServer, imageInfo.registry, imageInfo.name, imageInfo.tag)
	}

	return command
}

func (h *DockerHandler) isSignedImage(image string) (bool, string) {
	result, err := h.execToDockerDaemon(h.makeCommand(getImageInfo(image)))
	if err != nil {
		log.Printf("Failed to execute command to docker daemon by %s", err)
	}

	if result.OutBuffer.Len() <= 0 {
		log.Panicf("Failed to get signature of image %s", image)
	}

	signatures, err := getSignatures(result.OutBuffer.String())
	if err != nil {
		log.Printf("Failed to get signature by %s", err)
		return false, ""
	}

	if h.hasMatchedSigner(signatures) {
		digest := getDigest(image, signatures)
		return true, digest
	}

	return false, ""
}

func (h *DockerHandler) hasMatchedSigner(signatures []Signature) bool {
	if len(h.signerPolicies) == 0 {
		return len(signatures) != 0
	}

	if len(signatures) == 0 {
		return false
	}

	key := signatures[0].getRepoAdminKey()

	for _, signerPolicy := range h.signerPolicies {
		for _, signerName := range signerPolicy.Spec.Signers {
			signer := &regv1.SignerKey{}
			if err := h.client.RESTClient().Get().AbsPath("apis/tmax.io/v1").Resource("signerkeys").Name(signerName).Do(context.TODO()).Into(signer); err != nil {
				log.Printf("signer getting error by %s", err)
			}

			for _, targetKey := range signer.Spec.Targets {
				if targetKey.ID == key {
					return true
				}
			}
		}
	}

	return false
}

func (h *DockerHandler) execToDockerDaemon(command string) (*ExecResult, error) {
	result := &ExecResult{
		OutBuffer: &bytes.Buffer{},
		ErrBuffer: &bytes.Buffer{},
	}

	if err := k8s.ExecCmd(h.dindPodName, dindContainer, dindNamespace, command, nil, result.OutBuffer, result.ErrBuffer); err != nil {
		return result, err
	}

	return result, nil
}

func (h *DockerHandler) loginToRegistry(registry string) error {
	pullSecrets := h.pod.Spec.ImagePullSecrets
	if len(pullSecrets) <= 0 {
		return fmt.Errorf("There's any pullSecret")
	}

	for _, pullSecret := range pullSecrets {
		secret, err := h.getSecret(pullSecret.Name)
		if err != nil {
			log.Printf("Couldn't get secret named %s by %s", pullSecret.Name, err)
			break
		}
		id, idExist := secret.Data["ID"]
		pw, pwExist := secret.Data["PASSWD"]
		if idExist && pwExist {
			result, err := h.execToDockerDaemon(fmt.Sprintf("docker login %s -u %s -p %s", registry, id, pw))
			if err != nil {
				log.Printf("Couldn't exec docker login command by %s", err)
				continue
			}

			if strings.Contains(result.OutBuffer.String(), "Login Succeeded") {
				return nil
			}
		}
	}

	return fmt.Errorf("There's no pullSecret to login to registry named %s", registry)
}

func (h *DockerHandler) getSecret(secretName string) (*corev1.Secret, error) {
	allSecrets, err := h.client.CoreV1().Secrets("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var result corev1.Secret
	exist := false
	for _, secret := range allSecrets.Items {
		if secret.Name == secretName {
			result = secret
			exist = true
			break
		}
	}

	if exist {
		return &result, nil
	}

	return nil, fmt.Errorf("There's no secret named %s", secretName)
}

func (h *DockerHandler) isImageInWhiteList(image string) bool {
	imageInfo := getImageInfo(image)
	validFormatImage := fmt.Sprintf("%s/%s:%s", imageInfo.registry, imageInfo.name, imageInfo.tag)
	for _, whiteListImage := range h.whiteList.byImages {
		if strings.Contains(validFormatImage, whiteListImage) {
			return true
		}
	}

	return false
}

func (h *DockerHandler) isNamespaceInWhiteList() bool {
	for _, whiteListNamespace := range h.whiteList.byNamespaces {
		if h.pod.Namespace == whiteListNamespace {
			return true
		}
	}

	return false
}

func (h *DockerHandler) findNotaryServer(registry string) string {
	if registry == "docker.io" {
		return "docker.io"
	}

	var targetReg *regv1.Registry
	regList := h.getRegistries()
	for _, reg := range regList.Items {
		if "https://"+registry == reg.Status.ServerURL {
			targetReg = &reg
			break
		}
	}

	if targetReg == nil {
		log.Printf("No matched registry named: %s. Couldn't find notary server", registry)
		return "docker-hub"
	}

	return targetReg.Status.NotaryURL
}

func (h *DockerHandler) getRegistries() *regv1.RegistryList {
	regList := &regv1.RegistryList{}
	if err := h.client.RESTClient().Get().AbsPath("/apis/tmax.io/v1").Resource("registries").Do(context.TODO()).Into(regList); err != nil {
		log.Printf("reg list err %s", err)
	}

	return regList
}

func getImageInfo(image string) ImageInfo {
	var host, name, tag string

	if strings.Contains(image, "/") {
		idx := strings.LastIndex(image, "/")
		host = image[:idx]
		name = image[idx+1:]
	} else {
		host = "docker.io"
		name = image
	}

	if strings.Contains(name, ":") {
		temp := strings.Split(name, ":")
		name = temp[0]
		tag = temp[1]
	} else {
		tag = "latest"
	}

	log.Printf("INFO: image %s => registry: %s name: %s tag: %s", image, host, name, tag)

	return ImageInfo{
		registry: host,
		name:     name,
		tag:      tag,
	}
}

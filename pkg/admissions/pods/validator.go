package pods

import (
	"context"
	"fmt"

	"github.com/tmax-cloud/image-validating-webhook/internal/utils"
	"github.com/tmax-cloud/image-validating-webhook/pkg/notary"
	whv1 "github.com/tmax-cloud/image-validating-webhook/pkg/type"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var (
	validatorLog = logf.Log.WithName("validator")
)

func init() {
	if err := whv1.AddToScheme(scheme.Scheme); err != nil {
		validatorLog.Error(err, "")
	}
	if err := corev1.AddToScheme(scheme.Scheme); err != nil {
		validatorLog.Error(err, "")
	}
}

// Validator validates pods if the images are signed
type Validator interface {
	CheckIsValidAndAddDigest(pod *corev1.Pod) (bool, string, error)
}

// validator handles overall process to check signs
type validator struct {
	client kubernetes.Interface

	registryPolicyCache *RegistryPolicyCache
	whiteList           *WhiteList
}

func newValidator(cfg *rest.Config, clientSet kubernetes.Interface, restClient rest.Interface) (*validator, error) {
	v := &validator{
		client: clientSet,
	}

	var err error

	// Initiate RegistryPolicy cache
	v.registryPolicyCache, err = newRegistryPolicyCache(cfg, restClient)
	if err != nil {
		return nil, err
	}

	// Initiate WhiteList cache
	v.whiteList, err = newWhiteList(cfg, clientSet)
	if err != nil {
		return nil, err
	}

	return v, nil
}

// CheckIsValidAndAddDigest checks if images of initContainers and containers are valid
func (h *validator) CheckIsValidAndAddDigest(pod *corev1.Pod) (bool, string, error) {
	// Check namespace whitelist
	if h.whiteList.IsNamespaceWhiteListed(pod.Namespace) {
		return true, "", nil
	}

	// Check initContainers
	if isValid, reason, err := h.addDigestWhenImageValid(pod.Spec.InitContainers, pod.Namespace, pod.Spec.ImagePullSecrets); err != nil {
		return false, "", err
	} else if !isValid {
		return false, reason, nil
	}
	// Check containers
	if isValid, reason, err := h.addDigestWhenImageValid(pod.Spec.Containers, pod.Namespace, pod.Spec.ImagePullSecrets); err != nil {
		return false, "", err
	} else if !isValid {
		return false, reason, nil
	}

	return true, "", nil
}

func (h *validator) addDigestWhenImageValid(containers []corev1.Container, namespace string, pullSecrets []corev1.LocalObjectReference) (bool, string, error) {
	for i, container := range containers {
		// Check if it's whitelisted
		if h.whiteList.IsImageWhiteListed(container.Image) {
			continue
		}

		ref, err := parseImage(container.Image)
		if err != nil {
			return false, "", err
		}

		// Get registry basic auth
		basicAuth, err := h.getBasicAuthForRegistry(ref.host, namespace, pullSecrets)
		if err != nil {
			return false, "", err
		}

		// Check if it meets registry security policy
		if valid, policy := h.registryPolicyCache.doesMatchPolicy(ref.host, namespace); valid && policy.Registry == "" {
			return true, "", nil
		} else if valid {
			if !policy.SignCheck {
				return true, "", nil
			}
			// Get trust info of the image
			sig, err := notary.FetchSignature(container.Image, basicAuth, policy.Notary)
			if err != nil {
				validatorLog.Error(err, "")
				return false, "", err
			}
			// sig is nil if it's not signed
			if sig == nil {
				return false, fmt.Sprintf("Image '%s' is not signed", container.Image), nil
			}

			digest := sig.GetDigest(ref.tag)

			// If digest is different from user-specified one, return error
			if ref.digest != "" && ref.digest != digest {
				return false, fmt.Sprintf("Image '%s''s digest is different from the signed digest", container.Image), nil
			}

			ref.digest = digest
			containers[i].Image = ref.String()

			return true, "", nil
		}
		// Does NOT match registry security policy
		return false, fmt.Sprintf("Image '%s' does not meet registry security policy. Please check the RegistrySecurityPolicy", container.Image), nil
	}
	return true, "", nil
}

func (h *validator) getBasicAuthForRegistry(host, namespace string, pullSecrets []corev1.LocalObjectReference) (string, error) {
	for _, pullSecret := range pullSecrets {
		secret, err := h.client.CoreV1().Secrets(namespace).Get(context.Background(), pullSecret.Name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("couldn't get secret named %s by %s", pullSecret.Name, err)
		}
		imagePullSecret, err := utils.NewImagePullSecret(secret)
		if err != nil {
			return "", err
		}
		basicAuth, err := imagePullSecret.GetHostBasicAuth(h.findRegistryServer(host))
		if err != nil {
			return "", err
		}
		if basicAuth == "" {
			continue
		}

		return basicAuth, nil
	}

	// DO NOT return error - the image may be public
	return "", nil
}

func (h *validator) findRegistryServer(registry string) string {
	if registry == "docker.io" {
		return "https://registry-1.docker.io"
	}
	return "https://" + registry
}

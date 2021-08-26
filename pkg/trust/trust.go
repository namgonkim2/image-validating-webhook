package trust

import (
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	regclient "github.com/docker/distribution/registry/client"
	"github.com/docker/distribution/registry/client/auth/challenge"
	"github.com/fvbommel/sortorder"
	"github.com/theupdateframework/notary"
	"github.com/theupdateframework/notary/client"
	"github.com/theupdateframework/notary/trustpinning"
	"github.com/theupdateframework/notary/tuf/data"
	"github.com/tmax-cloud/image-validating-webhook/internal/utils"
	"github.com/tmax-cloud/image-validating-webhook/pkg/auth"
	"github.com/tmax-cloud/image-validating-webhook/pkg/image"
	ctrl "sigs.k8s.io/controller-runtime"
)

var log = ctrl.Log.WithName("trust")

var (
	// ReleasesRole is the role named "releases"
	ReleasesRole = data.RoleName(path.Join(data.CanonicalTargetsRole.String(), "releases"))
)

// trustTagKey represents a unique signed tag and hex-encoded hash pair
type trustTagKey struct {
	SignedTag string
	Digest    string
}

// trustTagRow encodes all human-consumable information for a signed tag, including signers
type trustTagRow struct {
	trustTagKey
	Signers []string
}

// trustRepo represents consumable information about a trusted repository
type trustRepo struct {
	Name               string
	SignedTags         []trustTagRow
	Signers            []trustSigner
	AdministrativeKeys []trustSigner
}

// trustSigner represents a trusted signer in a trusted repository
// a signer is defined by a name and list of trustKeys
type trustSigner struct {
	Name string     `json:",omitempty"`
	Keys []trustKey `json:",omitempty"`
}

// trustKey contains information about trusted keys
type trustKey struct {
	ID string `json:",omitempty"`
}

// ReadOnly can get sign data
type ReadOnly interface {
	GetSignedMetadata(string) (*trustRepo, error)
	ClearDir() error
}

// TrustPass key-value map to store passPhrase
type TrustPass map[string]string

// AddKeyPass add new passPhrase to TrustPass
func (p TrustPass) AddKeyPass(key, val string) {
	p[key] = val
}

type notaryRepo struct {
	notaryPath      string
	notaryServerURL string
	repo            client.Repository
	token           *auth.Token
	image           *image.Image
	passPhrase      TrustPass
}

const (
	// DefaultNotaryServer is url of docker hub's notary server
	DefaultNotaryServer = "https://notary.docker.io"
	releasedRoleName    = "Repo Admin"
)

// NewReadOnly returns new readonly object to get sign data
func NewReadOnly(image *image.Image, notaryURL, path string) (ReadOnly, error) {
	n := &notaryRepo{
		notaryPath: path,
		image:      image,
	}

	// Notary Server url
	if notaryURL == "" {
		n.notaryServerURL = DefaultNotaryServer
	} else {
		n.notaryServerURL = notaryURL
	}

	token, err := n.GetToken()
	if err != nil {
		return nil, err
	}

	// Generate Transport
	rt := &auth.RegistryTransport{
		Base: &http.Transport{ // Base is DefaultTransport, added TLSClientConfig
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		},
		Token: token,
	}

	// Initialize Notary repository
	repo, err := client.NewFileCachedRepository(n.notaryPath, data.GUN(image.GetImageNameWithHost()), n.notaryServerURL, rt, n.passRetriever(), trustpinning.TrustPinConfig{})
	if err != nil {
		return nil, err
	}
	n.repo = repo

	return n, nil
}

// GetToken returns token to get sign from notary server
func (n *notaryRepo) GetToken() (*auth.Token, error) {
	if n.token == nil || n.token.Type == "" || n.token.Value == "" {
		if err := n.fetchToken(); err != nil {
			log.Error(err, "")
			return nil, err
		}
	}

	return n.token, nil
}

func (n *notaryRepo) checkPingResponse(pingResp int) bool {
	if pingResp >= 200 && pingResp < 300 {
		if n.image.BasicAuth == "" {
			n.token = nil
		} else {
			n.token = &auth.Token{
				Type:  "Basic",
				Value: n.image.BasicAuth,
			}
		}
		return true
	}
	return false
}

func (n *notaryRepo) fetchToken() error {
	log.Info("Fetching token...")
	// Ping
	u, err := url.Parse(n.notaryServerURL)
	if err != nil {
		return err
	}
	u.Path = path.Join(u.Path, "v2")
	pingReq, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	if n.image.BasicAuth != "" {
		pingReq.Header.Set("Authorization", fmt.Sprintf("Basic %s", n.image.BasicAuth))
	}
	pingResp, err := n.image.HttpClient.Do(pingReq)
	if err != nil {
		return err
	}
	defer func() {
		_ = pingResp.Body.Close()
	}()
	// If 200, use basic auth

	if n.checkPingResponse(pingResp.StatusCode) {
		return nil
	}

	challenges := challenge.ResponseChallenges(pingResp)
	if len(challenges) < 1 {
		return fmt.Errorf("header does not contain WWW-Authenticate")
	}
	realm, realmExist := challenges[0].Parameters["realm"]
	service, serviceExist := challenges[0].Parameters["service"]
	if !realmExist || !serviceExist {
		return fmt.Errorf("there is no realm or service in parameters")
	}

	// Get Token
	return n.setToken(service, realm)
}

func (n *notaryRepo) setToken(service string, realm string) error {
	img := n.image.GetImageNameWithHost()

	param := map[string]string{
		"service": service,
		"scope":   fmt.Sprintf("repository:%s:pull,push", img),
	}
	tokenReq, err := http.NewRequest(http.MethodGet, realm, nil)
	if err != nil {
		return err
	}
	if n.image.BasicAuth != "" {
		tokenReq.Header.Set("Authorization", fmt.Sprintf("Basic %s", n.image.BasicAuth))
	}
	tokenQ := tokenReq.URL.Query()
	for k, v := range param {
		tokenQ.Add(k, v)
	}
	tokenReq.URL.RawQuery = tokenQ.Encode()

	tokenResp, err := n.image.HttpClient.Do(tokenReq)
	if err != nil {
		return err
	}
	defer func() {
		_ = tokenResp.Body.Close()
	}()
	if !regclient.SuccessStatus(tokenResp.StatusCode) {
		err := regclient.HandleErrorResponse(tokenResp)
		return err
	}

	decoder := json.NewDecoder(tokenResp.Body)
	token := &auth.TokenResponse{}
	if err := decoder.Decode(token); err != nil {
		return err
	}

	n.token = &auth.Token{
		Type:  "Bearer",
		Value: token.Token,
	}

	return nil
}

func (n *notaryRepo) passRetriever() notary.PassRetriever {
	return func(id, _ string, createNew bool, attempts int) (string, bool, error) {
		if createNew {
			n.passPhrase.AddKeyPass(id, utils.RandomString(10))
		}
		phrase, ok := n.passPhrase[id]
		if !ok {
			return "", attempts > 1, fmt.Errorf("no pass phrase is found")
		}
		return phrase, attempts > 1, nil
	}
}

// ClearDir remove temporary directory
func (n *notaryRepo) ClearDir() error {
	return os.RemoveAll(n.notaryPath)
}

// GetSignedMetadata returns trust repository
func (n *notaryRepo) GetSignedMetadata(tag string) (*trustRepo, error) {
	allSignedTargets, err := n.repo.GetAllTargetMetadataByName(tag)
	if err != nil {
		log.Error(err, "failed to get all target metadata")
		return &trustRepo{}, err
	}

	signatureRows := matchReleasedSignatures(allSignedTargets)

	// get the administrative roles
	adminRolesWithSigs, err := n.repo.ListRoles()
	if err != nil {
		return &trustRepo{}, fmt.Errorf("No signers for %s", n.notaryServerURL)
	}

	// get delegation roles with the canonical key IDs
	delegationRoles, err := n.repo.GetDelegationRoles()
	if err != nil {
		log.Error(err, "no delegation roles found, or error fetching them for %s", n.notaryServerURL)
	}

	// process the signatures to include repo admin if signed by the base targets role
	for idx, sig := range signatureRows {
		if len(sig.Signers) == 0 {
			signatureRows[idx].Signers = append(sig.Signers, releasedRoleName)
		}
	}

	signerList, adminList := []trustSigner{}, []trustSigner{}

	signerRoleToKeyIDs := getDelegationRoleToKeyMap(delegationRoles)

	for signerName, signerKeys := range signerRoleToKeyIDs {
		signerKeyList := []trustKey{}
		for _, keyID := range signerKeys {
			signerKeyList = append(signerKeyList, trustKey{ID: keyID})
		}
		signerList = append(signerList, trustSigner{signerName, signerKeyList})
	}
	sort.Slice(signerList, func(i, j int) bool { return signerList[i].Name > signerList[j].Name })

	for _, adminRole := range adminRolesWithSigs {
		switch adminRole.Name {
		case data.CanonicalRootRole:
			rootKeys := []trustKey{}
			for _, keyID := range adminRole.KeyIDs {
				rootKeys = append(rootKeys, trustKey{ID: keyID})
			}
			adminList = append(adminList, trustSigner{"Root", rootKeys})
		case data.CanonicalTargetsRole:
			targetKeys := []trustKey{}
			for _, keyID := range adminRole.KeyIDs {
				targetKeys = append(targetKeys, trustKey{ID: keyID})
			}
			adminList = append(adminList, trustSigner{"Repository", targetKeys})
		}
	}
	sort.Slice(adminList, func(i, j int) bool { return adminList[i].Name > adminList[j].Name })

	return &trustRepo{
		Name:               n.repo.GetGUN().String(),
		SignedTags:         signatureRows,
		Signers:            signerList,
		AdministrativeKeys: adminList,
	}, nil
}

func matchReleasedSignatures(allTargets []client.TargetSignedStruct) []trustTagRow {
	signatureRows := []trustTagRow{}
	// do a first pass to get filter on tags signed into "targets" or "targets/releases"
	releasedTargetRows := map[trustTagKey][]string{}
	for _, tgt := range allTargets {
		if isReleasedTarget(tgt.Role.Name) {
			releasedKey := trustTagKey{tgt.Target.Name, hex.EncodeToString(tgt.Target.Hashes[notary.SHA256])}
			releasedTargetRows[releasedKey] = []string{}
		}
	}

	// now fill out all signers on released keys
	for _, tgt := range allTargets {
		targetKey := trustTagKey{tgt.Target.Name, hex.EncodeToString(tgt.Target.Hashes[notary.SHA256])}
		// only considered released targets
		if _, ok := releasedTargetRows[targetKey]; ok && !isReleasedTarget(tgt.Role.Name) {
			releasedTargetRows[targetKey] = append(releasedTargetRows[targetKey], notaryRoleToSigner(tgt.Role.Name))
		}
	}

	// compile the final output as a sorted slice
	for targetKey, signers := range releasedTargetRows {
		signatureRows = append(signatureRows, trustTagRow{targetKey, signers})
	}
	sort.Slice(signatureRows, func(i, j int) bool {
		return sortorder.NaturalLess(signatureRows[i].SignedTag, signatureRows[j].SignedTag)
	})
	return signatureRows
}

func getDelegationRoleToKeyMap(rawDelegationRoles []data.Role) map[string][]string {
	signerRoleToKeyIDs := make(map[string][]string)
	for _, delRole := range rawDelegationRoles {
		switch delRole.Name {
		case ReleasesRole, data.CanonicalRootRole, data.CanonicalSnapshotRole, data.CanonicalTargetsRole, data.CanonicalTimestampRole:
			continue
		default:
			signerRoleToKeyIDs[notaryRoleToSigner(delRole.Name)] = delRole.KeyIDs
		}
	}
	return signerRoleToKeyIDs
}

// isReleasedTarget checks if a role name is "released":
// either targets/releases or targets TUF roles
func isReleasedTarget(role data.RoleName) bool {
	return role == data.CanonicalTargetsRole || role == ReleasesRole
}

// notaryRoleToSigner converts TUF role name to a human-understandable signer name
func notaryRoleToSigner(tufRole data.RoleName) string {
	//  don't show a signer for "targets" or "targets/releases"
	if isReleasedTarget(data.RoleName(tufRole.String())) {
		return releasedRoleName
	}
	return strings.TrimPrefix(tufRole.String(), "targets/")
}

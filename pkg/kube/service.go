package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pborman/uuid"
	"github.com/pkg/errors"
	"github.com/technosophos/moniker"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubejson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/versioning"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	clientcmdlatest "k8s.io/client-go/tools/clientcmd/api/latest"
	"k8s.io/helm/pkg/helm"
	"k8s.io/helm/pkg/proto/hapi/chart"
	"k8s.io/helm/pkg/proto/hapi/release"
	"k8s.io/helm/pkg/timeconv"

	"github.com/supergiant/control/pkg/model"
	"github.com/supergiant/control/pkg/runner/ssh"
	"github.com/supergiant/control/pkg/sgerrors"
	"github.com/supergiant/control/pkg/sghelm/proxy"
	"github.com/supergiant/control/pkg/storage"
	"github.com/supergiant/control/pkg/workflows/steps/kubelet"
)

const (
	KubernetesAdminUser = "kubernetes-admin"

	DefaultStoragePrefix = "/supergiant/kubes/"

	releaseInstallTimeout = 300
)

var (
	ErrNoHelmProxy = errors.New("helm proxy constructor not found")

	_ Interface = &Service{}
)

// Interface represents an interface for a kube service.
type Interface interface {
	Create(ctx context.Context, k *model.Kube) error
	Get(ctx context.Context, name string) (*model.Kube, error)
	ListAll(ctx context.Context) ([]model.Kube, error)
	Delete(ctx context.Context, name string) error
	KubeConfigFor(ctx context.Context, kname, user string) ([]byte, error)
	ListKubeResources(ctx context.Context, kname string) ([]byte, error)
	GetKubeResources(ctx context.Context, kname, resource, ns, name string) ([]byte, error)
	ListNodes(ctx context.Context, k *model.Kube, role string) ([]corev1.Node, error)
	GetCerts(ctx context.Context, kname, cname string) (*Bundle, error)
	InstallRelease(ctx context.Context, kname string, rls *ReleaseInput) (*release.Release, error)
	ListReleases(ctx context.Context, kname, ns, offset string, limit int) ([]*model.ReleaseInfo, error)
	ReleaseDetails(ctx context.Context, kname, rlsName string) (*release.Release, error)
	DeleteRelease(ctx context.Context, kname, rlsName string, purge bool) (*model.ReleaseInfo, error)
}

// ChartGetter interface is a wrapper for GetChart function.
type ChartGetter interface {
	GetChart(ctx context.Context, repoName, chartName, chartVersion string) (*chart.Chart, error)
}

type ServerResourceGetter interface {
	ServerResources() ([]*metav1.APIResourceList, error)
}

// Service manages kubernetes clusters.
type Service struct {
	discoveryClientFn func(k *model.Kube) (ServerResourceGetter, error)
	corev1ClientFn    func(k *model.Kube) (corev1client.CoreV1Interface, error)
	clientForGroupFn  func(k *model.Kube, gv schema.GroupVersion) (rest.Interface, error)

	prefix  string
	storage storage.Interface

	newHelmProxyFn func(kube *model.Kube) (proxy.Interface, error)
	chrtGetter     ChartGetter
}

// NewService constructs a Service.
func NewService(prefix string, s storage.Interface, chrtGetter ChartGetter) *Service {
	return &Service{
		clientForGroupFn: restClientForGroupVersion,
		corev1ClientFn:   corev1Client,
		newHelmProxyFn:   helmProxyFrom,
		chrtGetter:       chrtGetter,
		prefix:           prefix,
		storage:          s,
	}
}

// Create and stores a kube in the provided storage.
func (s Service) Create(ctx context.Context, k *model.Kube) error {
	if k.ID == "" {
		k.ID = uuid.New()[:8]
	}

	raw, err := json.Marshal(k)
	if err != nil {
		return errors.Wrap(err, "marshal")
	}

	err = s.storage.Put(ctx, s.prefix, k.ID, raw)
	if err != nil {
		return errors.Wrap(err, "storage: put")
	}

	return nil
}

// Get returns a kube with a specified name.
func (s Service) Get(ctx context.Context, kubeID string) (*model.Kube, error) {
	raw, err := s.storage.Get(ctx, s.prefix, kubeID)
	if err != nil {
		return nil, errors.Wrap(err, "storage: get")
	}
	if raw == nil {
		return nil, sgerrors.ErrNotFound
	}

	k := &model.Kube{}
	if err = json.Unmarshal(raw, k); err != nil {
		return nil, errors.Wrap(err, "unmarshal")
	}

	return k, nil
}

// ListAll returns all kubes.
func (s Service) ListAll(ctx context.Context) ([]model.Kube, error) {
	rawKubes, err := s.storage.GetAll(ctx, s.prefix)
	if err != nil {
		return nil, errors.Wrap(err, "storage: getAll")
	}

	kubes := make([]model.Kube, len(rawKubes))
	for i, v := range rawKubes {
		k := model.Kube{}
		if err = json.Unmarshal(v, &k); err != nil {
			return nil, errors.Wrap(err, "unmarshal")
		}
		kubes[i] = k
	}

	return kubes, nil
}

// Delete deletes a kube with a specified name.
func (s Service) Delete(ctx context.Context, kubeID string) error {
	return s.storage.Delete(ctx, s.prefix, kubeID)
}

// ListKubeResources returns raw representation of the supported kubernetes resources.
func (s Service) ListKubeResources(ctx context.Context, kubeID string) ([]byte, error) {
	kube, err := s.Get(ctx, kubeID)
	if err != nil {
		return nil, errors.Wrap(err, "get kube")
	}

	resourcesInfo, err := s.resourcesGroupInfo(kube)
	if err != nil {
		return nil, err
	}

	raw, err := json.Marshal(resourcesInfo)
	if err != nil {
		return nil, errors.Wrap(err, "marshal")
	}

	return raw, nil
}

// GetKubeResources returns raw representation of the kubernetes resources.
func (s Service) GetKubeResources(ctx context.Context, kubeID, resource, ns, name string) ([]byte, error) {
	kube, err := s.Get(ctx, kubeID)
	if err != nil {
		return nil, errors.Wrap(err, "get kube")
	}

	resourcesInfo, err := s.resourcesGroupInfo(kube)
	if err != nil {
		return nil, err
	}

	gv, ok := resourcesInfo[resource]
	if !ok {
		return nil, sgerrors.ErrNotFound
	}

	client, err := s.clientForGroupFn(kube, gv)
	if err != nil {
		return nil, errors.Wrap(err, "get kube client")
	}

	req := client.Get().Resource(resource).Namespace(ns)
	if name != "" {
		req.Name(name)
	}
	raw, err := req.DoRaw()
	if err != nil {
		return nil, errors.Wrap(err, "get resources")
	}

	return raw, nil
}

func (s Service) ListNodes(ctx context.Context, kube *model.Kube, role string) ([]corev1.Node, error) {
	if s.corev1ClientFn == nil {
		return nil, errors.Wrap(sgerrors.ErrNilEntity, "corev1client builder")
	}
	kclient, err := s.corev1ClientFn(kube)
	if err != nil {
		return nil, err
	}
	nodeList, err := kclient.Nodes().List(metav1.ListOptions{
		LabelSelector: toRoleSelector(role),
	})
	if err != nil {
		return nil, err
	}
	return nodeList.Items, nil
}

func (s Service) KubeConfigFor(ctx context.Context, kubeID, user string) ([]byte, error) {
	// there are certificates only for the cluster-admin user
	if user != KubernetesAdminUser {
		return nil, errors.Wrapf(sgerrors.ErrNotFound, "%q user", user)
	}

	kube, err := s.Get(ctx, kubeID)
	if err != nil {
		return nil, errors.Wrapf(err, "get %s model", kubeID)
	}

	kubeconfig, err := adminKubeConfig(kube)
	if err != nil {
		return nil, err
	}

	serializer := kubejson.NewSerializer(kubejson.DefaultMetaFactory, clientcmdlatest.Scheme, clientcmdlatest.Scheme, false)
	codec := versioning.NewDefaultingCodecForScheme(
		clientcmdlatest.Scheme,
		serializer,
		serializer,
		schema.GroupVersion{Version: clientcmdlatest.Version},
		runtime.InternalGroupVersioner,
	)
	return runtime.Encode(codec, &kubeconfig)
}

// GetCerts returns a keys bundle for provided component name.
// TODO: do we need this?
func (s Service) GetCerts(ctx context.Context, kname, cname string) (*Bundle, error) {
	kube, err := s.Get(ctx, kname)
	if err != nil {
		return nil, err
	}

	// TODO(stgleb): pass host info here
	r, err := ssh.NewRunner(ssh.Config{
		User: kube.SSHConfig.User,
		Key:  []byte(kube.SSHConfig.PublicKey),
	})
	if err != nil {
		return nil, errors.Wrap(err, "setup runner")
	}

	certs, err := NewCerts(DefaultCertsPath, r)
	if err != nil {
		return nil, errors.Wrap(err, "setup certs getter")
	}

	b, err := certs.BundleFor(ctx, cname)
	if err != nil {
		return nil, errors.Wrap(err, "get keys bundle")
	}

	return b, nil
}

func (s Service) InstallRelease(ctx context.Context, kubeID string, rls *ReleaseInput) (*release.Release, error) {
	if rls == nil {
		return nil, errors.Wrap(sgerrors.ErrNilEntity, "release input")
	}

	chrt, err := s.chrtGetter.GetChart(ctx, rls.RepoName, rls.ChartName, rls.ChartVersion)
	if err != nil {
		return nil, errors.Wrap(err, "get chart")
	}

	kube, err := s.Get(ctx, kubeID)
	if err != nil {
		return nil, errors.Wrap(err, "get kube")
	}
	kprx, err := s.helmClient(kube)
	if err != nil {
		return nil, errors.Wrap(err, "build helm proxy")
	}

	rr, err := kprx.InstallReleaseFromChart(
		chrt,
		rls.Namespace,
		helm.ReleaseName(ensureReleaseName(rls.Name)),
		helm.ValueOverrides([]byte(rls.Values)),
		helm.InstallWait(false),
		helm.InstallTimeout(releaseInstallTimeout),
	)

	return rr.GetRelease(), err
}

func (s Service) ReleaseDetails(ctx context.Context, kubeID, rlsName string) (*release.Release, error) {
	kube, err := s.Get(ctx, kubeID)
	if err != nil {
		return nil, errors.Wrap(err, "get kube")
	}
	kprx, err := s.helmClient(kube)
	if err != nil {
		return nil, errors.Wrap(err, "build helm proxy")
	}

	rr, err := kprx.ReleaseContent(rlsName)
	if err != nil {
		return nil, errors.Wrap(err, "get release details")
	}

	return rr.GetRelease(), nil
}

func (s Service) ListReleases(ctx context.Context, kubeID, namespace, offset string, limit int) ([]*model.ReleaseInfo, error) {
	kube, err := s.Get(ctx, kubeID)
	if err != nil {
		return nil, errors.Wrap(err, "get kube")
	}
	kprx, err := s.helmClient(kube)
	if err != nil {
		return nil, errors.Wrap(err, "build helm proxy")
	}

	res, err := kprx.ListReleases(
		helm.ReleaseListNamespace(namespace),
		helm.ReleaseListOffset(offset),
		helm.ReleaseListLimit(limit),
		helm.ReleaseListStatuses(releaseStatuses()),
	)
	if err != nil {
		return nil, errors.Wrap(err, "list releases")
	}
	out := make([]*model.ReleaseInfo, 0, len(res.GetReleases()))
	for _, rls := range res.GetReleases() {
		if rls != nil {
			out = append(out, toReleaseInfo(rls))
		}
	}

	return out, nil
}

func (s Service) DeleteRelease(ctx context.Context, kubeID, rlsName string, purge bool) (*model.ReleaseInfo, error) {
	kube, err := s.Get(ctx, kubeID)
	if err != nil {
		return nil, errors.Wrap(err, "get kube")
	}
	kprx, err := s.helmClient(kube)
	if err != nil {
		return nil, errors.Wrap(err, "build helm proxy")
	}

	res, err := kprx.DeleteRelease(
		rlsName,
		helm.DeletePurge(purge),
	)
	if err != nil {
		return nil, errors.Wrap(err, "delete releases")
	}

	return toReleaseInfo(res.GetRelease()), nil
}

func (s Service) helmClient(k *model.Kube) (proxy.Interface, error) {
	if s.newHelmProxyFn == nil {
		return nil, ErrNoHelmProxy
	}
	return s.newHelmProxyFn(k)
}

func (s Service) resourcesGroupInfo(kube *model.Kube) (map[string]schema.GroupVersion, error) {
	client, err := s.discoveryClientFn(kube)
	if err != nil {
		return nil, errors.Wrap(err, "get discovery client")
	}

	apiResourceLists, err := client.ServerResources()
	if err != nil {
		return nil, errors.Wrap(err, "get resources")
	}

	resourcesGroupInfo := map[string]schema.GroupVersion{}
	for _, apiResourceList := range apiResourceLists {
		gv, err := schema.ParseGroupVersion(apiResourceList.GroupVersion)
		if err != nil {
			continue
		}

		for _, apiResource := range apiResourceList.APIResources {
			if _, ok := resourcesGroupInfo[apiResource.Kind]; !ok {
				resourcesGroupInfo[apiResource.Name] = gv
			}
		}
	}

	return resourcesGroupInfo, nil
}

func ensureReleaseName(name string) string {
	if strings.TrimSpace(name) == "" {
		return moniker.New().NameSep("-")
	}
	return name
}

func toReleaseInfo(rls *release.Release) *model.ReleaseInfo {
	if rls == nil {
		return nil
	}
	return &model.ReleaseInfo{
		Name:         rls.GetName(),
		Namespace:    rls.GetNamespace(),
		Version:      rls.GetVersion(),
		CreatedAt:    timeconv.String(rls.GetInfo().GetFirstDeployed()),
		LastDeployed: timeconv.String(rls.GetInfo().GetLastDeployed()),
		Chart:        rls.GetChart().Metadata.Name,
		ChartVersion: rls.GetChart().Metadata.Version,
		Status:       rls.GetInfo().Status.Code.String(),
	}
}

func releaseStatuses() []release.Status_Code {
	// TODO: filter releases by statuses on the UI side?
	return []release.Status_Code{
		release.Status_UNKNOWN,
		release.Status_DEPLOYED,
		release.Status_DELETED,
		release.Status_DELETING,
		release.Status_FAILED,
		release.Status_PENDING_INSTALL,
		release.Status_PENDING_UPGRADE,
		release.Status_PENDING_ROLLBACK,
	}
}

func toRoleSelector(role string) string {
	if role != "" {
		return fmt.Sprintf("%s=%s", kubelet.LabelNodeRole, role)
	}
	return ""
}

// Copyright (C) 2017 Percona LLC
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

// Package kubernetes provides functionality for kubernetes.
package kubernetes

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/AlekSi/pointer"
	victoriametricsv1beta1 "github.com/VictoriaMetrics/operator/api/v1beta1"
	"github.com/gen1us2k/everest-provisioner/data"
	"github.com/gen1us2k/everest-provisioner/kubernetes/client"
	"github.com/operator-framework/api/pkg/operators/v1alpha1"

	dbaasv1 "github.com/percona/dbaas-operator/api/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apimachinery/pkg/version"
)

type ClusterType string

const (
	ClusterTypeUnknown         ClusterType = "unknown"
	ClusterTypeMinikube        ClusterType = "minikube"
	ClusterTypeEKS             ClusterType = "eks"
	ClusterTypeGeneric         ClusterType = "generic"
	pxcDeploymentName                      = "percona-xtradb-cluster-operator"
	psmdbDeploymentName                    = "percona-server-mongodb-operator"
	dbaasDeploymentName                    = "dbaas-operator-controller-manager"
	psmdbOperatorContainerName             = "percona-server-mongodb-operator"
	pxcOperatorContainerName               = "percona-xtradb-cluster-operator"
	dbaasOperatorContainerName             = "manager"
	databaseClusterKind                    = "DatabaseCluster"
	databaseClusterAPIVersion              = "dbaas.percona.com/v1"
	restartAnnotationKey                   = "dbaas.percona.com/restart"
	managedByKey                           = "dbaas.percona.com/managed-by"
	templateLabelKey                       = "dbaas.percona.com/template"
	engineLabelKey                         = "dbaas.percona.com/engine"

	// ContainerStateWaiting represents a state when container requires some
	// operations being done in order to complete start up.
	ContainerStateWaiting ContainerState = "waiting"
	// ContainerStateTerminated indicates that container began execution and
	// then either ran to completion or failed for some reason.
	ContainerStateTerminated ContainerState = "terminated"

	// Max size of volume for AWS Elastic Block Storage service is 16TiB.
	maxVolumeSizeEBS    uint64 = 16 * 1024 * 1024 * 1024 * 1024
	olmNamespace               = "olm"
	useDefaultNamespace        = ""

	// APIVersionCoreosV1 constant for some API requests.
	APIVersionCoreosV1 = "operators.coreos.com/v1"

	pollInterval = 1 * time.Second
	pollDuration = 5 * time.Minute
)

// ErrEmptyVersionTag Got an empty version tag from GitHub API.
var ErrEmptyVersionTag error = errors.New("got an empty version tag from Github")

// Kubernetes is a client for Kubernetes.
type Kubernetes struct {
	lock       *sync.RWMutex
	client     client.KubeClientConnector
	l          *logrus.Entry
	httpClient *http.Client
	kubeconfig string
}

// ContainerState describes container's state - waiting, running, terminated.
type ContainerState string

// NodeSummaryNode holds information about Node inside Node's summary.
type NodeSummaryNode struct {
	FileSystem NodeFileSystemSummary `json:"fs,omitempty"`
}

// NodeSummary holds summary of the Node.
// One gets this by requesting Kubernetes API endpoint:
// /v1/nodes/<node-name>/proxy/stats/summary.
type NodeSummary struct {
	Node NodeSummaryNode `json:"node,omitempty"`
}

// NodeFileSystemSummary holds a summary of Node's filesystem.
type NodeFileSystemSummary struct {
	UsedBytes uint64 `json:"usedBytes,omitempty"`
}

// New returns new Kubernetes object.
func New(kubeconfig string) (*Kubernetes, error) {
	l := logrus.WithField("component", "kubernetes")

	client, err := client.NewFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}

	return &Kubernetes{
		client: client,
		l:      l,
		lock:   &sync.RWMutex{},
		httpClient: &http.Client{
			Timeout: time.Second * 5,
			Transport: &http.Transport{
				MaxIdleConns:    1,
				IdleConnTimeout: 10 * time.Second,
			},
		},
		kubeconfig: kubeconfig,
	}, nil
}

// NewEmpty returns new Kubernetes object.
func NewEmpty() *Kubernetes {
	return &Kubernetes{
		client: &client.Client{},
		lock:   &sync.RWMutex{},
		l:      logrus.WithField("component", "kubernetes"),
		httpClient: &http.Client{
			Timeout: time.Second * 5,
			Transport: &http.Transport{
				MaxIdleConns:    1,
				IdleConnTimeout: 10 * time.Second,
			},
		},
	}
}

// GetKubeconfig generates kubeconfig compatible with kubectl for incluster created clients.
func (k *Kubernetes) GetKubeconfig(ctx context.Context) (string, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	secret, err := k.client.GetSecretsForServiceAccount(ctx, "pmm-service-account")
	if err != nil {
		k.l.Errorf("failed getting service account: %v", err)
		return "", err
	}

	kubeConfig, err := k.client.GenerateKubeConfig(secret)
	if err != nil {
		k.l.Errorf("failed generating kubeconfig: %v", err)
		return "", err
	}

	return string(kubeConfig), nil
}

// ListDatabaseClusters returns list of managed PCX clusters.
func (k *Kubernetes) ListDatabaseClusters(ctx context.Context) (*dbaasv1.DatabaseClusterList, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	return k.client.ListDatabaseClusters(ctx)
}

// GetDatabaseCluster returns PXC clusters by provided name.
func (k *Kubernetes) GetDatabaseCluster(ctx context.Context, name string) (*dbaasv1.DatabaseCluster, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	return k.client.GetDatabaseCluster(ctx, name)
}

// RestartDatabaseCluster restarts database cluster
func (k *Kubernetes) RestartDatabaseCluster(ctx context.Context, name string) error {
	k.lock.Lock()
	defer k.lock.Unlock()
	cluster, err := k.client.GetDatabaseCluster(ctx, name)
	if err != nil {
		return err
	}
	cluster.TypeMeta.APIVersion = databaseClusterAPIVersion
	cluster.TypeMeta.Kind = databaseClusterKind
	if cluster.ObjectMeta.Annotations == nil {
		cluster.ObjectMeta.Annotations = make(map[string]string)
	}
	cluster.ObjectMeta.Annotations[restartAnnotationKey] = "true"
	return k.client.ApplyObject(cluster)
}

// PatchDatabaseCluster patches CR of managed Database cluster.
func (k *Kubernetes) PatchDatabaseCluster(cluster *dbaasv1.DatabaseCluster) error {
	k.lock.Lock()
	defer k.lock.Unlock()
	return k.client.ApplyObject(cluster)
}

// CreateDatabaseCluster creates database cluster
func (k *Kubernetes) CreateDatabaseCluster(cluster *dbaasv1.DatabaseCluster) error {
	k.lock.Lock()
	defer k.lock.Unlock()
	if cluster.ObjectMeta.Annotations == nil {
		cluster.ObjectMeta.Annotations = make(map[string]string)
	}
	cluster.ObjectMeta.Annotations[managedByKey] = "pmm"
	return k.client.ApplyObject(cluster)
}

// DeleteDatabaseCluster deletes database cluster
func (k *Kubernetes) DeleteDatabaseCluster(ctx context.Context, name string) error {
	k.lock.Lock()
	defer k.lock.Unlock()
	cluster, err := k.client.GetDatabaseCluster(ctx, name)
	if err != nil {
		return err
	}
	cluster.TypeMeta.APIVersion = databaseClusterAPIVersion
	cluster.TypeMeta.Kind = databaseClusterKind
	return k.client.DeleteObject(cluster)
}

// GetDefaultStorageClassName returns first storageClassName from kubernetes cluster
func (k *Kubernetes) GetDefaultStorageClassName(ctx context.Context) (string, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	storageClasses, err := k.client.GetStorageClasses(ctx)
	if err != nil {
		return "", err
	}
	if len(storageClasses.Items) != 0 {
		return storageClasses.Items[0].Name, nil
	}
	return "", errors.New("no storage classes available")
}

// GetClusterType tries to guess the underlying kubernetes cluster based on storage class
func (k *Kubernetes) GetClusterType(ctx context.Context) (ClusterType, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	storageClasses, err := k.client.GetStorageClasses(ctx)
	if err != nil {
		return ClusterTypeUnknown, err
	}
	for _, storageClass := range storageClasses.Items {
		if strings.Contains(storageClass.Provisioner, "aws") {
			return ClusterTypeEKS, nil
		}
		if strings.Contains(storageClass.Provisioner, "minikube") ||
			strings.Contains(storageClass.Provisioner, "kubevirt.io/hostpath-provisioner") ||
			strings.Contains(storageClass.Provisioner, "standard") {
			return ClusterTypeMinikube, nil
		}
	}
	return ClusterTypeGeneric, nil
}

// getOperatorVersion parses operator version from operator deployment
func (k *Kubernetes) getOperatorVersion(ctx context.Context, deploymentName, containerName string) (string, error) {
	deployment, err := k.client.GetDeployment(ctx, deploymentName)
	if err != nil {
		return "", err
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name == containerName {
			return strings.Split(container.Image, ":")[1], nil
		}
	}
	return "", errors.New("unknown version of operator")
}

// GetPSMDBOperatorVersion parses PSMDB operator version from operator deployment
func (k *Kubernetes) GetPSMDBOperatorVersion(ctx context.Context) (string, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	return k.getOperatorVersion(ctx, psmdbDeploymentName, psmdbOperatorContainerName)
}

// GetPXCOperatorVersion parses PXC operator version from operator deployment
func (k *Kubernetes) GetPXCOperatorVersion(ctx context.Context) (string, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	return k.getOperatorVersion(ctx, pxcDeploymentName, pxcOperatorContainerName)
}

// GetDBaaSOperatorVersion parses DBaaS operator version from operator deployment
func (k *Kubernetes) GetDBaaSOperatorVersion(ctx context.Context) (string, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	return k.getOperatorVersion(ctx, dbaasDeploymentName, dbaasOperatorContainerName)
}

// GetSecret returns secret by name
func (k *Kubernetes) GetSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	return k.client.GetSecret(ctx, name)
}

// ListSecrets returns secret by name
func (k *Kubernetes) ListSecrets(ctx context.Context) (*corev1.SecretList, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	return k.client.ListSecrets(ctx)
}

// CreatePMMSecret creates pmm secret in kubernetes.
func (k *Kubernetes) CreatePMMSecret(secretName string, secrets map[string][]byte) error {
	k.lock.Lock()
	defer k.lock.Unlock()
	secret := &corev1.Secret{ //nolint: exhaustruct
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: secretName,
		},
		Type: corev1.SecretTypeOpaque,
		Data: secrets,
	}
	return k.client.ApplyObject(secret)
}

func (k *Kubernetes) CreateRestore(restore *dbaasv1.DatabaseClusterRestore) error {
	k.lock.Lock()
	defer k.lock.Unlock()
	return k.client.ApplyObject(restore)
}

// GetPods returns list of pods.
func (k *Kubernetes) GetPods(ctx context.Context, namespace string, labelSelector *metav1.LabelSelector) (*corev1.PodList, error) {
	return k.client.GetPods(ctx, namespace, labelSelector)
}

// GetLogs returns logs as slice of log lines - strings - for given pod's container.
func (k *Kubernetes) GetLogs(
	ctx context.Context,
	containerStatuses []corev1.ContainerStatus,
	pod,
	container string,
) ([]string, error) {
	if IsContainerInState(containerStatuses, ContainerStateWaiting) {
		return []string{}, nil
	}

	stdout, err := k.client.GetLogs(ctx, pod, container)
	if err != nil {
		return nil, errors.Wrap(err, "couldn't get logs")
	}

	if stdout == "" {
		return []string{}, nil
	}

	return strings.Split(stdout, "\n"), nil
}

// GetEvents returns pod's events as a slice of strings.
func (k *Kubernetes) GetEvents(ctx context.Context, pod string) ([]string, error) {
	stdout, err := k.client.GetEvents(ctx, pod)
	if err != nil {
		return nil, errors.Wrap(err, "couldn't describe pod")
	}

	lines := strings.Split(stdout, "\n")

	return lines, nil
}

// IsContainerInState returns true if container is in give state, otherwise false.
func IsContainerInState(containerStatuses []corev1.ContainerStatus, state ContainerState) bool {
	containerState := make(map[string]interface{})
	for _, status := range containerStatuses {
		data, err := json.Marshal(status.State)
		if err != nil {
			return false
		}

		if err := json.Unmarshal(data, &containerState); err != nil {
			return false
		}

		if _, ok := containerState[string(state)]; ok {
			return true
		}
	}

	return false
}

// IsNodeInCondition returns true if node's condition given as an argument has
// status "True". Otherwise it returns false.
func IsNodeInCondition(node corev1.Node, conditionType corev1.NodeConditionType) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Status == corev1.ConditionTrue && condition.Type == conditionType {
			return true
		}
	}
	return false
}

// GetWorkerNodes returns list of cluster workers nodes.
func (k *Kubernetes) GetWorkerNodes(ctx context.Context) ([]corev1.Node, error) {
	nodes, err := k.client.GetNodes(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "could not get nodes of Kubernetes cluster")
	}
	forbidenTaints := map[string]corev1.TaintEffect{
		"node.cloudprovider.kubernetes.io/uninitialized": corev1.TaintEffectNoSchedule,
		"node.kubernetes.io/unschedulable":               corev1.TaintEffectNoSchedule,
		"node-role.kubernetes.io/master":                 corev1.TaintEffectNoSchedule,
	}
	workers := make([]corev1.Node, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		if len(node.Spec.Taints) == 0 {
			workers = append(workers, node)
			continue
		}
		for _, taint := range node.Spec.Taints {
			effect, keyFound := forbidenTaints[taint.Key]
			if !keyFound || effect != taint.Effect {
				workers = append(workers, node)
			}
		}
	}
	return workers, nil
}

// GetPersistentVolumes returns list of persistent volumes.
func (k *Kubernetes) GetPersistentVolumes(ctx context.Context) (*corev1.PersistentVolumeList, error) {
	return k.client.GetPersistentVolumes(ctx)
}

// GetStorageClasses returns all storage classes available in the cluster.
func (k *Kubernetes) GetStorageClasses(ctx context.Context) (*storagev1.StorageClassList, error) {
	return k.client.GetStorageClasses(ctx)
}

// InstallOLMOperator installs the OLM in the Kubernetes cluster.
func (k *Kubernetes) InstallOLMOperator(ctx context.Context) error {
	deployment, err := k.client.GetDeployment(ctx, "olm-operator")
	if err == nil && deployment != nil && deployment.ObjectMeta.Name != "" {
		return nil // already installed
	}

	var crdFile, olmFile, perconaCatalog []byte

	crdFile, err = fs.ReadFile(data.OLMCRDs, "crds/olm/crds.yaml")
	if err != nil {
		return errors.Wrapf(err, "failed to read OLM CRDs file")
	}

	if err := k.client.ApplyFile(crdFile); err != nil {
		return errors.Wrapf(err, "cannot apply %q file", crdFile)
	}

	olmFile, err = fs.ReadFile(data.OLMCRDs, "crds/olm/olm.yaml")
	if err != nil {
		return errors.Wrapf(err, "failed to read OLM file")
	}

	if err := k.client.ApplyFile(olmFile); err != nil {
		return errors.Wrapf(err, "cannot apply %q file", crdFile)
	}

	perconaCatalog, err = fs.ReadFile(data.OLMCRDs, "crds/olm/percona-dbaas-catalog.yaml")
	if err != nil {
		return errors.Wrapf(err, "failed to read percona catalog yaml file")
	}

	if err := k.client.ApplyFile(perconaCatalog); err != nil {
		return errors.Wrapf(err, "cannot apply %q file", crdFile)
	}

	if err := k.client.DoRolloutWait(ctx, types.NamespacedName{Namespace: olmNamespace, Name: "olm-operator"}); err != nil {
		return errors.Wrap(err, "error while waiting for deployment rollout")
	}
	if err := k.client.DoRolloutWait(ctx, types.NamespacedName{Namespace: "olm", Name: "catalog-operator"}); err != nil {
		return errors.Wrap(err, "error while waiting for deployment rollout")
	}

	crdResources, err := decodeResources(crdFile)
	if err != nil {
		return errors.Wrap(err, "cannot decode crd resources")
	}

	olmResources, err := decodeResources(olmFile)
	if err != nil {
		return errors.Wrap(err, "cannot decode olm resources")
	}

	resources := append(crdResources, olmResources...)

	subscriptions := filterResources(resources, func(r unstructured.Unstructured) bool {
		return r.GroupVersionKind() == schema.GroupVersionKind{
			Group:   v1alpha1.GroupName,
			Version: v1alpha1.GroupVersion,
			Kind:    v1alpha1.SubscriptionKind,
		}
	})

	for _, sub := range subscriptions {
		subscriptionKey := types.NamespacedName{Namespace: sub.GetNamespace(), Name: sub.GetName()}
		log.Printf("Waiting for subscription/%s to install CSV", subscriptionKey.Name)
		csvKey, err := k.client.GetSubscriptionCSV(ctx, subscriptionKey)
		if err != nil {
			return fmt.Errorf("subscription/%s failed to install CSV: %v", subscriptionKey.Name, err)
		}
		log.Printf("Waiting for clusterserviceversion/%s to reach 'Succeeded' phase", csvKey.Name)
		if err := k.client.DoCSVWait(ctx, csvKey); err != nil {
			return fmt.Errorf("clusterserviceversion/%s failed to reach 'Succeeded' phase", csvKey.Name)
		}
	}

	if err := k.client.DoRolloutWait(ctx, types.NamespacedName{Namespace: "olm", Name: "packageserver"}); err != nil {
		return errors.Wrap(err, "error while waiting for deployment rollout")
	}

	return nil
}

func decodeResources(f []byte) (objs []unstructured.Unstructured, err error) {
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(f), 8)
	for {
		var u unstructured.Unstructured
		err = dec.Decode(&u)
		if err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		objs = append(objs, u)
	}

	return objs, nil
}

func filterResources(resources []unstructured.Unstructured, filter func(unstructured.
	Unstructured) bool,
) (filtered []unstructured.Unstructured) {
	for _, r := range resources {
		if filter(r) {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

// InstallOperatorRequest holds the fields to make an operator install request.
type InstallOperatorRequest struct {
	Namespace              string
	Name                   string
	OperatorGroup          string
	CatalogSource          string
	CatalogSourceNamespace string
	Channel                string
	InstallPlanApproval    v1alpha1.Approval
	StartingCSV            string
}

// InstallOperator installs an operator via OLM.
func (k *Kubernetes) InstallOperator(ctx context.Context, req InstallOperatorRequest) error {
	if err := createOperatorGroupIfNeeded(ctx, k.client, req.OperatorGroup); err != nil {
		return err
	}

	subs, err := k.client.CreateSubscriptionForCatalog(ctx, req.Namespace, req.Name, "olm", req.CatalogSource,
		req.Name, req.Channel, req.StartingCSV, v1alpha1.ApprovalManual)
	if err != nil {
		return errors.Wrap(err, "cannot create a susbcription to install the operator")
	}

	err = wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		k.lock.Lock()
		defer k.lock.Unlock()

		subs, err = k.client.GetSubscription(ctx, req.Namespace, req.Name)
		if err != nil || subs == nil || (subs != nil && subs.Status.Install == nil) {
			return false, err
		}

		return true, nil
	})

	if err != nil {
		return err
	}
	if subs == nil {
		return fmt.Errorf("cannot get an install plan for the operator subscription: %q", req.Name)
	}

	ip, err := k.client.GetInstallPlan(ctx, req.Namespace, subs.Status.Install.Name)
	if err != nil {
		return err
	}

	ip.Spec.Approved = true
	_, err = k.client.UpdateInstallPlan(ctx, req.Namespace, ip)

	return err
}

func createOperatorGroupIfNeeded(ctx context.Context, client client.KubeClientConnector, name string) error {
	_, err := client.GetOperatorGroup(ctx, useDefaultNamespace, name)
	if err == nil {
		return nil
	}

	_, err = client.CreateOperatorGroup(ctx, "default", name)

	return err
}

// ListSubscriptions all the subscriptions in the namespace.
func (k *Kubernetes) ListSubscriptions(ctx context.Context, namespace string) (*v1alpha1.SubscriptionList, error) {
	return k.client.ListSubscriptions(ctx, namespace)
}

// UpgradeOperator upgrades an operator to the next available version.
func (k *Kubernetes) UpgradeOperator(ctx context.Context, namespace, name string) error {
	var subs *v1alpha1.Subscription

	// If the subscription was recently created, the install plan might not be ready yet.
	err := wait.Poll(pollInterval, pollDuration, func() (bool, error) {
		var err error
		subs, err = k.client.GetSubscription(ctx, namespace, name)
		if err != nil {
			return false, err
		}
		if subs == nil || subs.Status.Install == nil || subs.Status.Install.Name == "" {
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		return err
	}
	if subs == nil || subs.Status.Install == nil || subs.Status.Install.Name == "" {
		return fmt.Errorf("cannot get subscription for %q operator", name)
	}

	ip, err := k.client.GetInstallPlan(ctx, namespace, subs.Status.Install.Name)
	if err != nil {
		return errors.Wrapf(err, "cannot get install plan to upgrade %q", name)
	}

	if ip.Spec.Approved == true {
		return nil // There are no upgrades.
	}

	ip.Spec.Approved = true

	_, err = k.client.UpdateInstallPlan(ctx, namespace, ip)

	return err
}

// GetServerVersion returns server version
func (k *Kubernetes) GetServerVersion() (*version.Info, error) {
	return k.client.GetServerVersion()
}

// GetClusterServiceVersion retrieves a ClusterServiceVersion by namespaced name.
func (k *Kubernetes) GetClusterServiceVersion(ctx context.Context, key types.NamespacedName) (*v1alpha1.ClusterServiceVersion, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	return k.client.GetClusterServiceVersion(ctx, key)
}

// ListClusterServiceVersion list all CSVs for the given namespace.
func (k *Kubernetes) ListClusterServiceVersion(ctx context.Context, namespace string) (*v1alpha1.ClusterServiceVersionList, error) {
	k.lock.RLock()
	defer k.lock.RUnlock()
	return k.client.ListClusterServiceVersion(ctx, namespace)
}

// DeleteObject deletes an object.
func (k *Kubernetes) DeleteObject(obj runtime.Object) error {
	k.lock.RLock()
	defer k.lock.RUnlock()
	return k.client.DeleteObject(obj)
}

// and creates a VM Agent instance.
func (k *Kubernetes) ProvisionMonitoring(login, password, pmmPublicAddress string) error {
	randomCrypto, err := rand.Prime(rand.Reader, 64)
	if err != nil {
		return err
	}

	secretName := fmt.Sprintf("vm-operator-%d", randomCrypto)
	err = k.CreatePMMSecret(secretName, map[string][]byte{
		"username": []byte(login),
		"password": []byte(password),
	})
	if err != nil {
		return err
	}

	vmagent := vmAgentSpec(secretName, pmmPublicAddress)
	err = k.client.ApplyObject(vmagent)
	if err != nil {
		return errors.Wrap(err, "cannot apply vm agent spec")
	}

	files := []string{
		"crds/victoriametrics/crs/vmagent_rbac.yaml",
		"crds/victoriametrics/crs/vmnodescrape.yaml",
		"crds/victoriametrics/crs/vmpodscrape.yaml",
		"crds/victoriametrics/kube-state-metrics/service-account.yaml",
		"crds/victoriametrics/kube-state-metrics/cluster-role.yaml",
		"crds/victoriametrics/kube-state-metrics/cluster-role-binding.yaml",
		"crds/victoriametrics/kube-state-metrics/deployment.yaml",
		"crds/victoriametrics/kube-state-metrics/service.yaml",
		"crds/victoriametrics/kube-state-metrics.yaml",
	}
	for _, path := range files {
		file, err := data.OLMCRDs.ReadFile(path)
		if err != nil {
			return err
		}
		// retry 3 times because applying vmagent spec might take some time.
		for i := 0; i < 3; i++ {
			err = k.client.ApplyFile(file)
			if err != nil {
				time.Sleep(10 * time.Second)
				continue
			}
			break
		}
		if err != nil {
			return errors.Wrapf(err, "cannot apply file: %q", path)
		}
	}
	return nil
}

// CleanupMonitoring remove all files installed by ProvisionMonitoring.
func (k *Kubernetes) CleanupMonitoring() error {
	files := []string{
		"crds/victoriametrics/kube-state-metrics.yaml",
		"crds/victoriametrics/kube-state-metrics/cluster-role-binding.yaml",
		"crds/victoriametrics/kube-state-metrics/cluster-role.yaml",
		"crds/victoriametrics/kube-state-metrics/deployment.yaml",
		"crds/victoriametrics/kube-state-metrics/service-account.yaml",
		"crds/victoriametrics/kube-state-metrics/service.yaml",
		"crds/victoriametrics/crs/vmagent_rbac.yaml",
		"crds/victoriametrics/crs/vmnodescrape.yaml",
		"crds/victoriametrics/crs/vmpodscrape.yaml",
	}
	for _, path := range files {
		file, err := data.OLMCRDs.ReadFile(path)
		if err != nil {
			return err
		}
		err = k.client.DeleteFile(file)
		if err != nil {
			return errors.Wrapf(err, "cannot apply file: %q", path)
		}
	}

	return nil
}

func vmAgentSpec(secretName, address string) *victoriametricsv1beta1.VMAgent {
	return &victoriametricsv1beta1.VMAgent{
		TypeMeta: metav1.TypeMeta{
			Kind:       "VMAgent",
			APIVersion: "operator.victoriametrics.com/v1beta1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: "pmm-vmagent-" + secretName,
		},
		Spec: victoriametricsv1beta1.VMAgentSpec{
			ServiceScrapeNamespaceSelector: &metav1.LabelSelector{},
			ServiceScrapeSelector:          &metav1.LabelSelector{},
			PodScrapeNamespaceSelector:     &metav1.LabelSelector{},
			PodScrapeSelector:              &metav1.LabelSelector{},
			ProbeSelector:                  &metav1.LabelSelector{},
			ProbeNamespaceSelector:         &metav1.LabelSelector{},
			StaticScrapeSelector:           &metav1.LabelSelector{},
			StaticScrapeNamespaceSelector:  &metav1.LabelSelector{},
			ReplicaCount:                   pointer.ToInt32(1),
			SelectAllByDefault:             true,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("250m"),
					corev1.ResourceMemory: resource.MustParse("350Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("850Mi"),
				},
			},
			ExtraArgs: map[string]string{
				"memory.allowedPercent": "40",
			},
			RemoteWrite: []victoriametricsv1beta1.VMAgentRemoteWriteSpec{
				{
					URL: fmt.Sprintf("%s/victoriametrics/api/v1/write", address),
					TLSConfig: &victoriametricsv1beta1.TLSConfig{
						InsecureSkipVerify: true,
					},
					BasicAuth: &victoriametricsv1beta1.BasicAuth{
						Username: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: secretName,
							},
							Key: "username",
						},
						Password: corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: secretName,
							},
							Key: "password",
						},
					},
				},
			},
		},
	}
}

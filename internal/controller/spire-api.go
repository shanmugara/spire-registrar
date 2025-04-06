package controller

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/json"
	"net/http"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/yaml"
)

const (
	ClusterInfoCm              = "kubeadm-config"
	ClusterInfoCmNamespace     = "kube-system"
	SpireTrustDomainAnnotation = "omega.k8s.io/spire-trustdomain"
	APIServer                  = "omegaspire01.omegaworld.net"
	APIPort                    = 8080
	AdminKubeConfigSecret      = "admin-kubeconfig" // Name of the ConfigMap containing the admin kubeconfig
)

type SpireEntry struct {
	TrustDomain    string `json:"trustDomain,omitempty"`
	ServiceAccount string `json:"serviceAccount,omitempty"`
	Namespace      string `json:"namespace,omitempty"`
	Cluster        string `json:"cluster,omitempty"`
	KubeConfig     string `json:"kubeConfig,omitempty"`
}

type SpireEntryResponse struct {
	EntryID string `json:"entryID"`
	Message string `json:"message"`
}

type entryID string

type SpireAPI struct {
	Server string `json:"server"` // SPIRE server URL
	Port   int    `json:"port"`   // SPIRE server port
}

func (s *SpireAPI) GetServerURL() string {
	// Construct the full server URL
	if s.Port > 0 {
		return s.Server + ":" + fmt.Sprint(s.Port)
	}
	return s.Server
}

func (r *ServiceAccountReconciler) CreateEntry(ctx context.Context, sa *corev1.ServiceAccount) (*entryID, error) {
	logger := log.FromContext(ctx)
	logger.Info("Creating SPIRE entry for ServiceAccount", "name", sa.Name, "namespace", sa.Namespace)

	ClusterConfig, err := r.GetClusterInfo(ctx)
	if err != nil {
		logger.Error(err, "Failed to get cluster info from ConfigMap", "namespace", ClusterInfoCmNamespace, "name", ClusterInfoCm)
		return nil, err
	}

	clusterName := ClusterConfig["clusterName"]
	if clusterName == nil {
		logger.Error(fmt.Errorf("clusterName not found"), "Failed to find clusterName in ClusterConfiguration", "namespace", ClusterInfoCmNamespace, "name", ClusterInfoCm)
		return nil, fmt.Errorf("missing clusterName in configmap")
	}

	kubeConfigData, err := r.GetKubeConfig(ctx)
	if err != nil {
		logger.Error(err, "Failed to get kubeconfig. defaulting to empty string")
	}

	// Create the SpireEntry object based on the ServiceAccount and ConfigMap data
	se := SpireEntry{
		TrustDomain:    ClusterConfig["trustDomain"].(string),
		ServiceAccount: sa.Name,
		Namespace:      sa.Namespace,
		Cluster:        clusterName.(string),
		KubeConfig:     kubeConfigData,
	}

	api := SpireAPI{
		Server: fmt.Sprintf("http://%s", APIServer),
		Port:   APIPort,
	}
	apiUrl := api.GetServerURL()

	logger.Info("SPIRE API URL", "url", apiUrl)
	logger.Info("Creating SPIRE Entry", "entry", se)

	// Marshal the SpireEntry to JSON
	data, err := json.Marshal(se)
	if err != nil {
		logger.Error(err, "Failed to marshal SPIRE entry")
		return nil, err
	}
	// Send the request to the SPIRE server to create the entry
	logger.Info("Sending request to SPIRE server", "url", apiUrl, "data", string(data))

	resp, err := http.Post(apiUrl+"/v1/entries/add", "application/json", bytes.NewBuffer(data))

	if err != nil {
		logger.Error(err, "Failed to send request to SPIRE server", "url", "response", apiUrl, resp.Status)
		return nil, err
	}

	defer resp.Body.Close()

	var entry SpireEntryResponse
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Error(err, "Failed to read response body")
		return nil, err
	}
	if err := json.Unmarshal(respBody, &entry); err != nil {
		logger.Error(err, "Failed to unmarshal response body")
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		logger.Error(nil, "SPIRE server returned non-200 status code", "status", resp.Status)
		return nil, err
	} else {
		logger.Info("Successfully created SPIRE entry", "entryID", entry.EntryID)

	}
	eID := entryID(entry.EntryID)
	return &eID, nil
}

func (r *ServiceAccountReconciler) DeleteEntry(ctx context.Context, sa *corev1.ServiceAccount) error {
	logger := log.FromContext(ctx)
	logger.Info("Deleting SPIRE entry for ServiceAccount", "name", sa.Name, "namespace", sa.Namespace)

	ClusterConfig, err := r.GetClusterInfo(ctx)
	if err != nil {
		logger.Error(err, "Failed to get cluster info from ConfigMap", "namespace", ClusterInfoCmNamespace, "name", ClusterInfoCm)
		return err
	}

	se := SpireEntry{
		TrustDomain:    ClusterConfig["trustDomain"].(string),
		ServiceAccount: sa.Name,
		Namespace:      sa.Namespace,
		Cluster:        ClusterConfig["clusterName"].(string),
		KubeConfig:     "", // Not needed for deletion
	}

	api := SpireAPI{
		Server: fmt.Sprintf("http://%s", APIServer),
		Port:   APIPort,
	}
	apiUrl := api.GetServerURL()

	logger.Info("SPIRE API URL", "url", apiUrl)

	data, err := json.Marshal(se)
	if err != nil {
		logger.Error(err, "Failed to marshal SPIRE entry for deletion")
		return err
	}
	resp, err := http.Post(apiUrl+"/v1/entries/delete", "application/json", bytes.NewBuffer(data))
	if err != nil {
		logger.Error(err, "Failed deleting entry. spire-api returned a non-200", "url", apiUrl, "response", resp.Status)
		return err
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logger.Error(nil, "SPIRE server returned non-200 status code for deletion", "status", resp.Status)
		bodyBytes, _ := io.ReadAll(resp.Body)
		logger.Error(fmt.Errorf("response body: %s", string(bodyBytes)), "Failed to delete SPIRE entry")
		return fmt.Errorf("failed to delete SPIRE entry: %s", resp.Status)
	}

	logger.Info("Successfully deleted SPIRE entry")
	return nil
}

func (r *ServiceAccountReconciler) GetClusterInfo(ctx context.Context) (map[string]interface{}, error) {
	logger := log.FromContext(ctx)
	kacm := &corev1.ConfigMap{}

	if err := r.Get(ctx, client.ObjectKey{Namespace: ClusterInfoCmNamespace, Name: ClusterInfoCm}, kacm); err != nil {
		logger.Error(err, "Failed to get ConfigMap for cluster info", "namespace", ClusterInfoCmNamespace, "name", ClusterInfoCm)
		return nil, err
	}

	// Check if the ConfigMap has the required data
	if kacm.Data == nil || kacm.Annotations[SpireTrustDomainAnnotation] == "" {
		logger.Error(fmt.Errorf("invalid ConfigMap"), "missing trust-domain", "ConfigMap", ClusterInfoCm, "namespace", ClusterInfoCmNamespace)
		return nil, fmt.Errorf("missing required data in ConfigMap %s/%s", ClusterInfoCmNamespace, ClusterInfoCm)
	}

	trustDomain := kacm.Annotations[SpireTrustDomainAnnotation]

	var clusterInfo map[string]interface{}
	err := yaml.Unmarshal([]byte(kacm.Data["ClusterConfiguration"]), &clusterInfo)
	if err != nil {
		logger.Error(err, "Failed to unmarshal cluster info", "message", err.Error())
		return nil, err
	}
	// Inject the trust domain into the clusterInfo map for convenience
	clusterInfo["trustDomain"] = trustDomain
	return clusterInfo, nil
}

func (r *ServiceAccountReconciler) GetKubeConfig(ctx context.Context) (string, error) {
	logger := log.FromContext(ctx)
	logger.Info("Getting kubeconfig from Secret")
	kcSecret := &corev1.Secret{}

	var kubeConfig string
	if err := r.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: AdminKubeConfigSecret}, kcSecret); err != nil {
		logger.Error(err, "Failed to get Secret for kubeconfig", "namespace", "kube-system", "name", AdminKubeConfigSecret)
		return "", err
	}

	if kcSecret.Data == nil || len(kcSecret.Data) == 0 {
		logger.Error(fmt.Errorf("missing kubeconfig data"), "Failed to find kubeconfig in Secret", "namespace", "kube-system", "name", AdminKubeConfigSecret)
		return "", fmt.Errorf("missing kubeconfig data in Secret %s/%s", "kube-system", AdminKubeConfigSecret)
	} else {
		kubeConfig = base64.StdEncoding.EncodeToString(kcSecret.Data["kubeconfig"])
		logger.Info("Successfully retrieved kubeconfig")
		return kubeConfig, nil
	}

}

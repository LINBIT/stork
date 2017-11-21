package schedops

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/libopenstorage/openstorage/api"
	"github.com/portworx/sched-ops/k8s"
	"github.com/portworx/sched-ops/task"
	"github.com/portworx/torpedo/drivers/node"
	"github.com/portworx/torpedo/drivers/volume"
	"github.com/portworx/torpedo/pkg/errors"
)

const (
	// PXServiceName is the name of the portworx service
	PXServiceName = "portworx-service"
	// PXNamespace is the kubernetes namespace in which portworx daemon set runs.
	PXNamespace = "kube-system"
	// k8sPxRunningLabelKey is the label key used for px state
	k8sPxRunningLabelKey = "px/enabled"
	// k8sPxNotRunningLabelValue is label value for a not running px state
	k8sPxNotRunningLabelValue = "false"
	// k8sPodsRootDir is the directory under which k8s keeps all pods data
	k8sPodsRootDir = "/var/lib/kubelet/pods"
)

// errLabelPresent error type for a label being present on a node
type errLabelPresent struct {
	// label is the label key
	label string
	// node is the k8s node where the label is present
	node string
}

func (e *errLabelPresent) Error() string {
	return fmt.Sprintf("label %s is present on node %s", e.label, e.node)
}

type k8sSchedOps struct{}

func (k *k8sSchedOps) DisableOnNode(n node.Node) error {
	return k8s.Instance().AddLabelOnNode(n.Name, k8sPxRunningLabelKey, k8sPxNotRunningLabelValue)
}

func (k *k8sSchedOps) ValidateOnNode(n node.Node) error {
	return &errors.ErrNotSupported{
		Type:      "Function",
		Operation: "ValidateOnNode",
	}
}

func (k *k8sSchedOps) EnableOnNode(n node.Node) error {
	return k8s.Instance().RemoveLabelOnNode(n.Name, k8sPxRunningLabelKey)
}

func (k *k8sSchedOps) ValidateAddLabels(replicaNodes []api.Node, vol *api.Volume) error {
	pvc, ok := vol.Locator.VolumeLabels["pvc"]
	if !ok {
		return nil
	}

	var missingLabelNodes []string
	for _, rs := range replicaNodes {
		t := func() (interface{}, error) {
			n, err := k8s.Instance().GetNodeByName(rs.Id)
			if err == nil && n != nil {
				return n.Labels, nil
			}

			addrs := []string{rs.DataIp, rs.MgmtIp}
			n, err = k8s.Instance().SearchNodeByAddresses(addrs)
			if err == nil && n != nil {
				return n.Labels, nil
			}

			return nil, fmt.Errorf("failed to locate node using id: %s and addresses: %v", rs.Id, addrs)
		}

		ret, err := task.DoRetryWithTimeout(t, 1*time.Minute, 5*time.Second)
		if err != nil {
			return err
		}
		nodeLabels := ret.(map[string]string)
		if _, ok := nodeLabels[pvc]; !ok {
			missingLabelNodes = append(missingLabelNodes, rs.Id)
		}
	}

	if len(missingLabelNodes) > 0 {
		return &ErrLabelMissingOnNode{
			Label: pvc,
			Nodes: missingLabelNodes,
		}
	}
	return nil
}

func (k *k8sSchedOps) ValidateRemoveLabels(vol *volume.Volume) error {
	pvcLabel := vol.Name
	var staleLabelNodes []string
	for _, n := range node.GetWorkerNodes() {
		t := func() (interface{}, error) {
			nodeLabels, err := k8s.Instance().GetLabelsOnNode(n.Name)
			if err != nil {
				return nil, err
			}

			if _, ok := nodeLabels[pvcLabel]; ok {
				return nil, &errLabelPresent{
					node:  n.Name,
					label: pvcLabel,
				}
			}

			return nil, nil
		}

		_, err := task.DoRetryWithTimeout(t, 5*time.Minute, 5*time.Second)
		if err != nil {
			if _, ok := err.(*errLabelPresent); ok {
				staleLabelNodes = append(staleLabelNodes, n.Name)
			} else {
				return err
			}
		}
	}

	if len(staleLabelNodes) > 0 {
		return &ErrLabelNotRemovedFromNode{
			Label: pvcLabel,
			Nodes: staleLabelNodes,
		}
	}

	return nil
}

func (k *k8sSchedOps) GetVolumeName(vol *volume.Volume) string {
	if vol != nil && vol.ID != "" {
		return fmt.Sprintf("pvc-%s", vol.ID)
	}
	return ""
}

func (k *k8sSchedOps) ValidateVolumeCleanup(d node.Driver) error {
	nodeToPodsMap := make(map[string][]string)
	nodeMap := make(map[string]node.Node)

	connOpts := node.ConnectionOpts{
		Timeout:         1 * time.Minute,
		TimeBeforeRetry: 10 * time.Second,
	}
	listVolOpts := node.FindOpts{
		ConnectionOpts: connOpts,
		Name:           "*portworx-volume",
	}

	for _, n := range node.GetWorkerNodes() {
		volDirList, _ := d.FindFiles(k8sPodsRootDir, n, listVolOpts)
		nodeToPodsMap[n.Name] = separateFilePaths(volDirList)
		nodeMap[n.Name] = n
	}

	existingPods, _ := k8s.Instance().GetPods("")

	orphanPodsMap := make(map[string][]string)
	dirtyVolPodsMap := make(map[string][]string)

	for nodeName, volDirPaths := range nodeToPodsMap {
		var orphanPods []string
		var dirtyVolPods []string

		for _, path := range volDirPaths {
			podUID := extractPodUID(path)
			found := false
			for _, existingPod := range existingPods.Items {
				if podUID == string(existingPod.UID) {
					found = true
					break
				}
			}
			if found {
				continue
			}
			orphanPods = append(orphanPods, podUID)

			// Check if there are files under portworx volume
			// We use a depth of 2 because the files stored in the volume are in the pvc
			// directory under the portworx-volume folder for that pod. For instance,
			// ../kubernetes-io~portworx-volume/pvc-<id>/<all_user_files>
			n := nodeMap[nodeName]
			findFileOpts := node.FindOpts{
				ConnectionOpts: connOpts,
				MinDepth:       2,
				MaxDepth:       2,
			}
			files, _ := d.FindFiles(path, n, findFileOpts)
			if len(strings.TrimSpace(files)) > 0 {
				dirtyVolPods = append(dirtyVolPods, podUID)
			}
		}

		if len(orphanPods) > 0 {
			orphanPodsMap[nodeName] = orphanPods
			if len(dirtyVolPods) > 0 {
				dirtyVolPodsMap[nodeName] = dirtyVolPods
			}
		}
	}

	if len(orphanPodsMap) == 0 {
		return nil
	}
	return &ErrFailedToCleanupVolume{
		OrphanPods:   orphanPodsMap,
		DirtyVolPods: dirtyVolPodsMap,
	}
}

func (k *k8sSchedOps) GetServiceEndpoint() (string, error) {
	svc, err := k8s.Instance().GetService(PXServiceName, PXNamespace)
	if err == nil {
		return svc.Spec.ClusterIP, nil
	}
	return "", err
}

func separateFilePaths(volDirList string) []string {
	trimmedList := strings.TrimSpace(volDirList)
	if trimmedList == "" {
		return []string{}
	}
	return strings.Split(trimmedList, "\n")
}

func extractPodUID(volDirPath string) string {
	re := regexp.MustCompile(k8sPodsRootDir +
		"/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/.*")
	match := re.FindStringSubmatch(volDirPath)
	if len(match) > 1 {
		return match[1]
	}
	return ""
}

func init() {
	k := &k8sSchedOps{}
	Register("k8s", k)
}

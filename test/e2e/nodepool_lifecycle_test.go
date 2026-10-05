//go:build e2e
// +build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/example/nodepool-controller/test/utils"
)

var _ = Describe("NodePool lifecycle", Ordered, func() {
	const (
		pool = "e2e-gpu-pool"
		node = "e2e-synthetic-gpu"
	)

	AfterAll(func() {
		cmd := exec.Command("kubectl", "delete", "nodepool", pool, "--ignore-not-found")
		_, _ = utils.Run(cmd)
		cmd = exec.Command("kubectl", "delete", "node", node, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	It("joins a synthetic node, then removes it after NotReady", func() {
		By("creating the NodePool with a short grace period")
		manifest := fmt.Sprintf(`
apiVersion: nodes.example.com/v1alpha1
kind: NodePool
metadata:
  name: %s
spec:
  image: e2e
  labels:
    workload: gpu
  taints:
    - key: nvidia.com/gpu
      value: "true"
      effect: NoSchedule
  unreadyPolicy:
    gracePeriod: 5s
    action: Remove
    maxConcurrentRemovals: 1
    drainTimeout: 5s
`, pool)
		cmd := exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("creating a synthetic Node (not a real kind worker) with the bootstrap label")
		nodeYAML := fmt.Sprintf(`
apiVersion: v1
kind: Node
metadata:
  name: %s
  labels:
    nodes.example.com/nodepool: %s
    human: keep-me
spec: {}
`, node, pool)
		cmd = exec.Command("kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(nodeYAML)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred())

		By("marking the synthetic node Ready")
		patchReady(node, "True", "KubeletReady")

		By("waiting for pool labels and taints")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "node", node, "-o", "jsonpath={.metadata.labels.workload}")
			out, err := utils.Run(cmd)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(Equal("gpu"))
		}, 2*time.Minute, time.Second).Should(Succeed())

		By("spoofing Ready=Unknown (infra reclaim)")
		patchReady(node, "Unknown", "NodeStatusUnknown")

		By("waiting for the controller to delete the synthetic node")
		Eventually(func(g Gomega) {
			cmd := exec.Command("kubectl", "get", "node", node)
			_, err := utils.Run(cmd)
			g.Expect(err).To(HaveOccurred())
		}, 2*time.Minute, time.Second).Should(Succeed())
	})
})

func patchReady(name, status, reason string) {
	now := time.Now().UTC().Format(time.RFC3339)
	payload := fmt.Sprintf(`[
  {"op":"add","path":"/status/conditions","value":[
    {"type":"Ready","status":"%s","reason":"%s","message":"e2e",
     "lastHeartbeatTime":"%s","lastTransitionTime":"%s"}
  ]}
]`, status, reason, now, now)
	cmd := exec.Command("kubectl", "patch", "node", name, "--subresource=status", "--type=json", "-p", payload)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

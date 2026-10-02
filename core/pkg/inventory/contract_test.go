package inventory_test

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zlogic-labs/fleet/core/pkg/inventory"
)

const goldenPath = "testdata/report.v1.json"

func golden(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	return b
}

func fullReport() inventory.Report {
	at := time.Date(2026, 10, 2, 14, 30, 0, 0, time.UTC)
	return inventory.Report{
		Contract: inventory.ContractVersion,
		Cluster: inventory.Cluster{
			Name: "k3s-dev", Reachable: true, Version: "v1.36.4+k3s1",
			NodeCount: 2, ReadyNodes: 2, GPUCount: 9, ReadyGPUs: 9,
			CPUMillis: 32400000, MemoryMiB: 36798076, ReportedAt: at,
			Nodes: []inventory.Node{
				{
					Name: "desktop-jkf1khq", Ready: true,
					Roles:     []string{"control-plane", "worker"},
					GPU:       inventory.GPU{Model: "NVIDIA GeForce GT 720", Count: 1, TotalMemMiB: 2048},
					CPUMillis: 16000, AllocatableMemoryMiB: 36798076,
					Kubelet: "v1.36.4+k3s1", OSImage: "Ubuntu 24.04.1 LTS", ReportedAt: at,
					Addresses: map[string]string{"InternalIP": "172.31.74.144"},
				},
				{
					Name: "gpu-node-a", Ready: true, Roles: []string{"worker"},
					GPU:                  inventory.GPU{Model: "A100-SXM4-80GB", Count: 8, TotalMemMiB: 655360},
					CPUMillis:            128000,
					AllocatableMemoryMiB: 980000000, Kubelet: "v1.36.4", OSImage: "Ubuntu 22.04", ReportedAt: at,
					Addresses: map[string]string{"InternalIP": "10.0.0.5"},
				},
			},
		},
		Deployments: []inventory.Deployment{
			{
				Name: "qwen-0-5b", Namespace: "fleet", Model: "Qwen/Qwen2.5-0.5B-Instruct-GGUF",
				Desired: 1, Ready: 1, TP: 1, PP: 0, GPUPer: 1,
				Engine: "llama-cpp", State: inventory.DeployAvailable,
				Selector: "qwen-0.5b-gguf", Address: "10.43.19.18:8000",
				Version: "b11146-7fe450e19", Format: "gguf", ObservedAt: at,
			},
			{
				Name: "llama-70b", Namespace: "fleet", Model: "meta-llama/Llama-3.3-70B-Instruct",
				Desired: 2, Ready: 0, TP: 8, PP: 2, GPUPer: 16,
				Engine: "vllm", State: inventory.DeployInsufficientCapacity,
				Reason: "InsufficientCapacity: 1 of 16 GPUs schedulable",
				Format: "safetensors", ObservedAt: at,
			},
		},
	}
}

// The golden is the contract. If a json tag, a field name or an omitempty
// changes, this fails — which is the point of committing it rather than
// asserting against the struct, which would agree with itself.
func TestThisBuildMarshalsExactlyTheGolden(t *testing.T) {
	got, err := json.MarshalIndent(fullReport(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.TrimRight(golden(t), "\n")
	if !bytes.Equal(got, want) {
		t.Errorf("the wire format changed\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// The other direction: a field added to the struct but left out of the golden
// would make the test above pass while shipping a field nobody agreed on.
func TestTheGoldenCoversEveryField(t *testing.T) {
	var rep inventory.Report
	if err := json.Unmarshal(golden(t), &rep); err != nil {
		t.Fatalf("golden does not decode: %v", err)
	}
	got, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if want := bytes.TrimRight(golden(t), "\n"); !bytes.Equal(got, want) {
		t.Errorf("the golden does not round-trip\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Receivers must ignore fields they do not recognise, because during a rolling
// upgrade the operator is upgraded first about half the time. A receiver that
// rejected them would take the whole cluster's inventory down until both
// halves moved.
func TestAnUnknownFieldIsIgnoredRatherThanRejected(t *testing.T) {
	body := strings.Replace(string(golden(t)), `"contract": 1,`,
		`"contract": 1, "inventedByANewerOperator": {"anything": [1,2,3]},`, 1)

	var rep inventory.Report
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("an unknown field made the report undecodable: %v", err)
	}
	if rep.Cluster.Name != "k3s-dev" || len(rep.Deployments) != 2 {
		t.Errorf("the rest of the report did not survive: %+v", rep.Cluster.Name)
	}
}

// A deployment without a namespace and a name has no key, so two of them
// collide and the second silently overwrites the first in the console.
func TestValidateRejectsWhatCannotBeStored(t *testing.T) {
	cases := []struct {
		name  string
		build func(*inventory.Report)
		want  string
	}{
		{"an unnamed cluster", func(r *inventory.Report) { r.Cluster.Name = "" }, "cluster.name"},
		{"a deployment with no namespace", func(r *inventory.Report) { r.Deployments[0].Namespace = "" }, "namespace"},
		{"a deployment with no name", func(r *inventory.Report) { r.Deployments[0].Name = "" }, "name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep := fullReport()
			tc.build(&rep)
			err := rep.Validate()
			if err == nil {
				t.Fatalf("accepted a report it cannot store")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestTheGoldenIsValid(t *testing.T) {
	var rep inventory.Report
	if err := json.Unmarshal(golden(t), &rep); err != nil {
		t.Fatal(err)
	}
	if err := rep.Validate(); err != nil {
		t.Errorf("the golden is not a report the control plane would accept: %v", err)
	}
	if rep.Contract != inventory.ContractVersion {
		t.Errorf("golden speaks contract %d, this build speaks %d", rep.Contract, inventory.ContractVersion)
	}
}

package spectest

import (
	"io/ioutil"
	"reflect"
	"testing"

	"github.com/bazelbuild/rules_go/go/tools/bazel"
	"github.com/ghodss/yaml"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/epoch"
	pb "github.com/prysmaticlabs/prysm/proto/beacon/p2p/v1"
	"github.com/prysmaticlabs/prysm/shared/params/spectest"
	"github.com/prysmaticlabs/prysm/shared/testutil"
)

func runRegisteryProcessingTests(t *testing.T, filename string) {
	file, err := ioutil.ReadFile(filename)
	if err != nil {
		t.Fatalf("Could not load file %v", err)
	}

	s := &RegistryUpdatesMinimal{}
	if err := yaml.Unmarshal(file, s); err != nil {
		t.Fatalf("Failed to Unmarshal: %v", err)
	}

	if err := spectest.SetConfig(s.Config); err != nil {
		t.Fatal(err)
	}

	for _, tt := range s.TestCases {
		t.Run(tt.Description, func(t *testing.T) {
			preState := &pb.BeaconState{}
			if err := testutil.ConvertToPb(tt.Pre, preState); err != nil {
				t.Fatal(err)
			}

			var postState *pb.BeaconState
			postState, err = epoch.ProcessRegistryUpdates(preState)
			if err != nil {
				t.Fatal(err)
			}

			expectedPostState := &pb.BeaconState{}
			if err := testutil.ConvertToPb(tt.Post, expectedPostState); err != nil {
				t.Fatal(err)
			}

			if !reflect.DeepEqual(postState, expectedPostState) {
				t.Error("Did not get expected state")
			}
		})
	}
}

const registryUpdatesPrefix = "eth2_spec_tests/tests/epoch_processing/registry_updates/"

func TestRegistryProcessingMinimal(t *testing.T) {
	filepath, err := bazel.Runfile(registryUpdatesPrefix + "registry_updates_minimal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	runRegisteryProcessingTests(t, filepath)
}

func TestRegistryProcessingMainnet(t *testing.T) {
	filepath, err := bazel.Runfile(registryUpdatesPrefix + "registry_updates_mainnet.yaml")
	if err != nil {
		t.Fatal(err)
	}
	runRegisteryProcessingTests(t, filepath)
}

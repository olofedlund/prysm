package sync

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"github.com/libp2p/go-libp2p-core/peer"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	eth "github.com/prysmaticlabs/ethereumapis/eth/v1alpha1"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/blocks"
	"github.com/prysmaticlabs/prysm/beacon-chain/core/helpers"
	"github.com/prysmaticlabs/prysm/beacon-chain/p2p"
	"github.com/prysmaticlabs/prysm/beacon-chain/state"
	"github.com/prysmaticlabs/prysm/shared/bytesutil"
	"github.com/prysmaticlabs/prysm/shared/traceutil"
	"go.opencensus.io/trace"
)

// Validation
// - The block being voted for (attestation.data.beacon_block_root) passes validation.
// - The attestation's committee index (attestation.data.index) is for the correct subnet.
// - The attestation is unaggregated -- that is, it has exactly one participating validator (len(get_attesting_indices(state, attestation.data, attestation.aggregation_bits)) == 1).
// - attestation.data.slot is within the last ATTESTATION_PROPAGATION_SLOT_RANGE slots (attestation.data.slot + ATTESTATION_PROPAGATION_SLOT_RANGE >= current_slot >= attestation.data.slot).
// - The signature of attestation is valid.
func (s *Service) validateCommitteeIndexBeaconAttestation(ctx context.Context, pid peer.ID, msg *pubsub.Message) pubsub.ValidationResult {
	if pid == s.p2p.PeerID() {
		return pubsub.ValidationAccept
	}
	// Attestation processing requires the target block to be present in the database, so we'll skip
	// validating or processing attestations until fully synced.
	if s.initialSync.Syncing() {
		return pubsub.ValidationIgnore
	}
	ctx, span := trace.StartSpan(ctx, "sync.validateCommitteeIndexBeaconAttestation")
	defer span.End()

	if msg.Topic == nil {
		return pubsub.ValidationReject
	}

	// Override topic for decoding.
	originalTopic := msg.Topic
	format := p2p.GossipTypeMapping[reflect.TypeOf(&eth.Attestation{})]
	msg.Topic = &format

	m, err := s.decodePubsubMessage(msg)
	if err != nil {
		log.WithError(err).Debug("Could not decode message")
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationReject
	}
	// Restore topic.
	msg.Topic = originalTopic

	att, ok := m.(*eth.Attestation)
	if !ok {
		return pubsub.ValidationReject
	}

	if att.Data == nil {
		return pubsub.ValidationReject
	}
	// Attestation aggregation bits must exist.
	if att.AggregationBits == nil {
		return pubsub.ValidationReject
	}

	// Attestation's slot is within ATTESTATION_PROPAGATION_SLOT_RANGE.
	if err := helpers.ValidateAttestationTime(att.Data.Slot, s.chain.GenesisTime()); err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationIgnore
	}
	if helpers.SlotToEpoch(att.Data.Slot) != att.Data.Target.Epoch {
		return pubsub.ValidationReject
	}

	// Verify this the first attestation received for the participating validator for the slot.
	if s.hasSeenCommitteeIndicesSlot(att.Data.Slot, att.Data.CommitteeIndex, att.AggregationBits) {
		return pubsub.ValidationIgnore
	}

	// Reject an attestation if it references an invalid block.
	if s.hasBadBlock(bytesutil.ToBytes32(att.Data.BeaconBlockRoot)) ||
		s.hasBadBlock(bytesutil.ToBytes32(att.Data.Target.Root)) ||
		s.hasBadBlock(bytesutil.ToBytes32(att.Data.Source.Root)) {
		return pubsub.ValidationReject
	}

	// Verify the block being voted and the processed state is in DB and. The block should have passed validation if it's in the DB.
	blockRoot := bytesutil.ToBytes32(att.Data.BeaconBlockRoot)
	has, err := s.hasBlockAndState(ctx, blockRoot)
	if err != nil {
		return pubsub.ValidationIgnore
	}
	if !has {
		// A node doesn't have the block, it'll request from peer while saving the pending attestation to a queue.
		s.savePendingAtt(&eth.SignedAggregateAttestationAndProof{Message: &eth.AggregateAttestationAndProof{Aggregate: att}})
		return pubsub.ValidationIgnore
	}

	if err := s.chain.VerifyFinalizedConsistency(ctx, att.Data.BeaconBlockRoot); err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationReject
	}
	if err := s.chain.VerifyLmdFfgConsistency(ctx, att); err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationReject
	}

	preState, err := s.chain.AttestationPreState(ctx, att)
	if err != nil {
		log.WithError(err).Error("Could not to retrieve pre state")
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationIgnore
	}

	validationRes := s.validateUnaggregatedAttTopic(ctx, att, preState, *originalTopic)
	if validationRes != pubsub.ValidationAccept {
		return validationRes
	}

	validationRes = s.validateUnaggregatedAttWithState(ctx, att, preState)
	if validationRes != pubsub.ValidationAccept {
		return validationRes
	}

	s.setSeenCommitteeIndicesSlot(att.Data.Slot, att.Data.CommitteeIndex, att.AggregationBits)

	msg.ValidatorData = att

	return pubsub.ValidationAccept
}

// This validates beacon unaggregated attestation has correct topic string.
func (s *Service) validateUnaggregatedAttTopic(ctx context.Context, a *eth.Attestation, bs *state.BeaconState, t string) pubsub.ValidationResult {
	ctx, span := trace.StartSpan(ctx, "sync.validateUnaggregatedAttTopic")
	defer span.End()

	valCount, err := helpers.ActiveValidatorCount(bs, helpers.SlotToEpoch(a.Data.Slot))
	if err != nil {
		log.WithError(err).Error("Could not retrieve active validator count")
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationIgnore
	}
	count := helpers.SlotCommitteeCount(valCount)
	if a.Data.CommitteeIndex > count {
		return pubsub.ValidationReject
	}
	subnet := helpers.ComputeSubnetForAttestation(valCount, a)
	format := p2p.GossipTypeMapping[reflect.TypeOf(&eth.Attestation{})]
	digest, err := s.forkDigest()
	if err != nil {
		log.WithError(err).Error("Could not compute fork digest")
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationIgnore
	}
	if !strings.HasPrefix(t, fmt.Sprintf(format, digest, subnet)) {
		return pubsub.ValidationReject
	}

	return pubsub.ValidationAccept
}

// This validates beacon unaggregated attestation using the given state, the validation consists of bitfield length and count consistency
// and signature verification.
func (s *Service) validateUnaggregatedAttWithState(ctx context.Context, a *eth.Attestation, bs *state.BeaconState) pubsub.ValidationResult {
	ctx, span := trace.StartSpan(ctx, "sync.validateUnaggregatedAttWithState")
	defer span.End()

	committee, err := helpers.BeaconCommitteeFromState(bs, a.Data.Slot, a.Data.CommitteeIndex)
	if err != nil {
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationIgnore
	}

	// Verify number of aggregation bits matches the committee size.
	if err := helpers.VerifyBitfieldLength(a.AggregationBits, uint64(len(committee))); err != nil {
		return pubsub.ValidationReject
	}

	// Attestation must be unaggregated and the bit index must exist in the range of committee indices.
	// Note: eth2 spec suggests (len(get_attesting_indices(state, attestation.data, attestation.aggregation_bits)) == 1)
	// however this validation can be achieved without use of get_attesting_indices which is an O(n) lookup.
	if a.AggregationBits.Count() != 1 || a.AggregationBits.BitIndices()[0] >= len(committee) {
		return pubsub.ValidationReject
	}

	if err := blocks.VerifyAttestationSignature(ctx, bs, a); err != nil {
		log.WithError(err).Debug("Could not verify attestation")
		traceutil.AnnotateError(span, err)
		return pubsub.ValidationReject
	}

	return pubsub.ValidationAccept
}

// Returns true if the attestation was already seen for the participating validator for the slot.
func (s *Service) hasSeenCommitteeIndicesSlot(slot, committeeID uint64, aggregateBits []byte) bool {
	s.seenAttestationLock.RLock()
	defer s.seenAttestationLock.RUnlock()
	b := append(bytesutil.Bytes32(slot), bytesutil.Bytes32(committeeID)...)
	b = append(b, aggregateBits...)
	_, seen := s.seenAttestationCache.Get(string(b))
	return seen
}

// Set committee's indices and slot as seen for incoming attestations.
func (s *Service) setSeenCommitteeIndicesSlot(slot, committeeID uint64, aggregateBits []byte) {
	s.seenAttestationLock.Lock()
	defer s.seenAttestationLock.Unlock()
	b := append(bytesutil.Bytes32(slot), bytesutil.Bytes32(committeeID)...)
	b = append(b, aggregateBits...)
	s.seenAttestationCache.Add(string(b), true)
}

// hasBlockAndState returns true if the beacon node knows about a block and associated state in the
// database or cache.
func (s *Service) hasBlockAndState(ctx context.Context, blockRoot [32]byte) (bool, error) {
	hasStateSummary := s.stateSummaryCache.Has(blockRoot) || s.db.HasStateSummary(ctx, blockRoot)
	hasState, err := s.db.HasState(ctx, blockRoot)
	if err != nil {
		return false, err
	}
	hasState = hasState || hasStateSummary
	hasBlock := s.chain.HasInitSyncBlock(blockRoot) || s.db.HasBlock(ctx, blockRoot)
	return hasState && hasBlock, nil
}

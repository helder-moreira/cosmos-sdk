package types

import (
	"time"

	tmtypes "github.com/tendermint/tendermint/types"

	"github.com/cosmos/cosmos-sdk/codec"
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
	clienttypes "github.com/cosmos/cosmos-sdk/x/ibc/core/02-client/types"
	"github.com/cosmos/cosmos-sdk/x/ibc/core/exported"
)

// CheckMisbehaviourAndUpdateState determines whether or not two conflicting
// headers at the same height would have convinced the light client.
//
// NOTE: consensusState1 is the trusted consensus state that corresponds to the TrustedHeight
// of misbehaviour.Header1
// Similarly, consensusState2 is the trusted consensus state that corresponds
// to misbehaviour.Header2
func (cs ClientState) CheckMisbehaviourAndUpdateState(
	ctx sdk.Context,
	cdc codec.BinaryMarshaler,
	clientStore sdk.KVStore,
	misbehaviour exported.Misbehaviour,
) (exported.ClientState, error) {
	tmMisbehaviour, ok := misbehaviour.(*Misbehaviour)
	if !ok {
		return nil, sdkerrors.Wrapf(clienttypes.ErrInvalidClientType, "expected type %T, got %T", misbehaviour, &Misbehaviour{})
	}

	// If client is already frozen at earlier height than misbehaviour, return with error
	if cs.IsFrozen() && cs.FrozenHeight.LTE(misbehaviour.GetHeight()) {
		return nil, sdkerrors.Wrapf(clienttypes.ErrInvalidMisbehaviour,
			"client is already frozen at earlier height %s than misbehaviour height %s", cs.FrozenHeight, misbehaviour.GetHeight())
	}

	// Retrieve trusted consensus states for each Header in misbehaviour
	// and unmarshal from clientStore

	// Get consensus bytes from clientStore
	tmConsensusState1, err := GetConsensusState(clientStore, cdc, tmMisbehaviour.Header1.TrustedHeight)
	if err != nil {
		return nil, sdkerrors.Wrapf(err, "could not get trusted consensus state from clientStore for Header1 at TrustedHeight: %s", tmMisbehaviour.Header1)
	}

	// Get consensus bytes from clientStore
	tmConsensusState2, err := GetConsensusState(clientStore, cdc, tmMisbehaviour.Header2.TrustedHeight)
	if err != nil {
		return nil, sdkerrors.Wrapf(err, "could not get trusted consensus state from clientStore for Header2 at TrustedHeight: %s", tmMisbehaviour.Header2)
	}

	// calculate the age of the misbehaviour
	infractionTime := tmMisbehaviour.GetTime()
	ageDuration := ctx.BlockTime().Sub(infractionTime)

	var ageBlocks int64
	if tmMisbehaviour.GetHeight().GetEpochNumber() == cs.LatestHeight.VersionNumber {
		// if the misbehaviour is in the same version as the client then
		// perform expiry check using block height in addition to time
		infractionHeight := tmMisbehaviour.GetHeight().GetEpochHeight()
		ageBlocks = int64(cs.LatestHeight.VersionHeight - infractionHeight)
	} else {
		// if the misbehaviour is from a different version, then the version-height
		// of misbehaviour has no correlation with the current version-height
		// so we disable the block check by setting ageBlocks to 0 and only
		// rely on the time expiry check with ageDuration
		ageBlocks = 0
	}

	// TODO: Retrieve consensusparams from client state and not context
	// Issue #6516: https://github.com/cosmos/cosmos-sdk/issues/6516
	consensusParams := ctx.ConsensusParams()

	// Reject misbehaviour if the age is too old. Misbehaviour is considered stale
	// if the difference in time and number of blocks is greater than the allowed
	// parameters defined.
	//
	// NOTE: The first condition is a safety check as the consensus params cannot
	// be nil since the previous param values will be used in case they can't be
	// retrieved. If they are not set during initialization, Tendermint will always
	// use the default values.
	if consensusParams != nil &&
		consensusParams.Evidence != nil &&
		(ageDuration > consensusParams.Evidence.MaxAgeDuration ||
			ageBlocks > consensusParams.Evidence.MaxAgeNumBlocks) {
		return nil, sdkerrors.Wrapf(clienttypes.ErrInvalidMisbehaviour,
			"age duration (%s) and age blocks (%d) are greater than max consensus params for duration (%s) and block (%d)",
			ageDuration, ageBlocks, consensusParams.Evidence.MaxAgeDuration, consensusParams.Evidence.MaxAgeNumBlocks,
		)
	}

	// Check the validity of the two conflicting headers against their respective
	// trusted consensus states
	// NOTE: header height and commitment root assertions are checked in
	// misbehaviour.ValidateBasic by the client keeper and msg.ValidateBasic
	// by the base application.
	if err := checkMisbehaviourHeader(
		&cs, tmConsensusState1, tmMisbehaviour.Header1, ctx.BlockTime(),
	); err != nil {
		return nil, sdkerrors.Wrap(err, "verifying Header1 in Misbehaviour failed")
	}
	if err := checkMisbehaviourHeader(
		&cs, tmConsensusState2, tmMisbehaviour.Header2, ctx.BlockTime(),
	); err != nil {
		return nil, sdkerrors.Wrap(err, "verifying Header2 in Misbehaviour failed")
	}

	cs.FrozenHeight = tmMisbehaviour.GetHeight().(clienttypes.Height)
	return &cs, nil
}

// checkMisbehaviourHeader checks that a Header in Misbehaviour is valid misbehaviour given
// a trusted ConsensusState
func checkMisbehaviourHeader(
	clientState *ClientState, consState *ConsensusState, header *Header, currentTimestamp time.Time,
) error {

	tmTrustedValset, err := tmtypes.ValidatorSetFromProto(header.TrustedValidators)
	if err != nil {
		return sdkerrors.Wrap(err, "trusted validator set is not tendermint validator set type")
	}

	tmCommit, err := tmtypes.CommitFromProto(header.Commit)
	if err != nil {
		return sdkerrors.Wrap(err, "commit is not tendermint commit type")
	}

	// check the trusted fields for the header against ConsensusState
	if err := checkTrustedHeader(header, consState); err != nil {
		return err
	}

	// assert that the timestamp is not from more than an unbonding period ago
	if currentTimestamp.Sub(consState.Timestamp) >= clientState.UnbondingPeriod {
		return sdkerrors.Wrapf(
			ErrUnbondingPeriodExpired,
			"current timestamp minus the latest consensus state timestamp is greater than or equal to the unbonding period (%d >= %d)",
			currentTimestamp.Sub(consState.Timestamp), clientState.UnbondingPeriod,
		)
	}

	chainID := clientState.GetChainID()
	// If chainID is in version format, then set version number of chainID with the version number
	// of the misbehaviour header
	if clienttypes.IsEpochFormat(chainID) {
		chainID, _ = clienttypes.SetEpochNumber(chainID, header.GetHeight().GetEpochNumber())
	}

	// - ValidatorSet must have 2/3 similarity with trusted FromValidatorSet
	// - ValidatorSets on both headers are valid given the last trusted ValidatorSet
	if err := tmTrustedValset.VerifyCommitLightTrusting(
		chainID, tmCommit, clientState.TrustLevel.ToTendermint(),
	); err != nil {
		return sdkerrors.Wrapf(clienttypes.ErrInvalidMisbehaviour, "validator set in header has too much change from trusted validator set: %v", err)
	}
	return nil
}

// Copyright (C) 2019-2021 Algorand, Inc.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package apply

import (
	"fmt"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/tools/teal/converter"
)

// Allocate the map of LocalStates if it is nil, and return a copy. We do *not*
// call clone on each AppLocalState -- callers must do that for any values
// where they intend to modify a contained reference type e.g. KeyValue.
func cloneAppLocalStates(m map[basics.AppIndex]basics.AppLocalState) map[basics.AppIndex]basics.AppLocalState {
	res := make(map[basics.AppIndex]basics.AppLocalState, len(m))
	for k, v := range m {
		res[k] = v
	}
	return res
}

// Allocate the map of AppParams if it is nil, and return a copy. We do *not*
// call clone on each AppParams -- callers must do that for any values where
// they intend to modify a contained reference type e.g. the GlobalState.
func cloneAppParams(m map[basics.AppIndex]basics.AppParams) map[basics.AppIndex]basics.AppParams {
	res := make(map[basics.AppIndex]basics.AppParams, len(m))
	for k, v := range m {
		res[k] = v
	}
	return res
}

// getAppParams fetches the creator address and AppParams for the app index,
// if they exist. It does *not* clone the AppParams, so the returned params
// must not be modified directly.
func getAppParams(balances Balances, aidx basics.AppIndex) (params basics.AppParams, creator basics.Address, exists bool, err error) {
	creator, exists, err = balances.GetCreator(basics.CreatableIndex(aidx), basics.AppCreatable)
	if err != nil {
		return
	}

	// App doesn't exist. Not an error, but return straight away
	if !exists {
		return
	}

	record, err := balances.Get(creator, false)
	if err != nil {
		return
	}

	params, ok := record.AppParams[aidx]
	if !ok {
		// This should never happen. If app exists then we should have
		// found the creator successfully.
		err = fmt.Errorf("app %d not found in account %s", aidx, creator.String())
		return
	}

	return
}

func applyStateDelta(kv basics.TealKeyValue, stateDelta basics.StateDelta) error {
	if kv == nil {
		return fmt.Errorf("cannot apply delta to nil TealKeyValue")
	}

	// Because the keys of stateDelta each correspond to one existing/new
	// key in the key/value store, there can be at most one delta per key.
	// Therefore the order that the deltas are applied does not matter.
	for key, valueDelta := range stateDelta {
		switch valueDelta.Action {
		case basics.SetUintAction:
			kv[key] = basics.TealValue{
				Type: basics.TealUintType,
				Uint: valueDelta.Uint,
			}
		case basics.SetBytesAction:
			kv[key] = basics.TealValue{
				Type:  basics.TealBytesType,
				Bytes: valueDelta.Bytes,
			}
		case basics.DeleteAction:
			delete(kv, key)
		default:
			return fmt.Errorf("unknown delta action %d", valueDelta.Action)
		}
	}
	return nil
}

// applyError is an error type that may be returned by applyEvalDelta in case
// the transaction execution should not fail for a clear state program. This is
// to distinguish failures due to schema violations from failures due to system
// faults (e.g. a failed database read).
type applyError struct {
	msg string
}

func (a *applyError) Error() string {
	return a.msg
}

func isApplyError(err error) bool {
	_, ok := err.(*applyError)
	return ok
}

// applyEvalDelta applies a basics.EvalDelta to the app's global key/value
// store as well as a set of local key/value stores. If this function returns
// an error, the transaction must not be committed.
//
// If the delta we are applying was generated by a ClearStateProgram, then a
// failure to apply the delta does not necessarily mean the transaction should
// be rejected. For example, if the delta would exceed a state schema, then
// we don't want to apply the changes, but we also don't want to fail. In these
// situations, we return an applyError.
func applyEvalDelta(ac *transactions.ApplicationCallTxnFields, evalDelta basics.EvalDelta, params basics.AppParams, creator, sender basics.Address, balances Balances, appIdx basics.AppIndex) error {
	/*
	 * 1. Apply GlobalState delta (if any), allocating the key/value store
	 *    if required.
	 */

	proto := balances.ConsensusParams()
	if len(evalDelta.GlobalDelta) > 0 {
		// Clone the parameters so that they are safe to modify
		params = params.Clone()

		// Allocate GlobalState if necessary. We need to do this now
		// since an empty map will be read as nil from disk
		if params.GlobalState == nil {
			params.GlobalState = make(basics.TealKeyValue)
		}

		// Check that the global state delta isn't breaking any rules regarding
		// key/value lengths
		err := evalDelta.GlobalDelta.Valid(&proto)
		if err != nil {
			return &applyError{fmt.Sprintf("cannot apply GlobalState delta: %v", err)}
		}

		// Apply the GlobalDelta in place on the cloned copy
		err = applyStateDelta(params.GlobalState, evalDelta.GlobalDelta)
		if err != nil {
			return err
		}

		// Make sure we haven't violated the GlobalStateSchema
		err = params.GlobalState.SatisfiesSchema(params.GlobalStateSchema)
		if err != nil {
			return &applyError{fmt.Sprintf("GlobalState for app %d would use too much space: %v", appIdx, err)}
		}
	}

	/*
	 * 2. Apply each LocalState delta, fail fast if any affected account
	 *    has not opted in to appIdx or would violate the LocalStateSchema.
	 *    Don't write anything back to the cow yet.
	 */

	changes := make(map[basics.Address]basics.AppLocalState, len(evalDelta.LocalDeltas))
	for accountIdx, delta := range evalDelta.LocalDeltas {
		// LocalDeltas are keyed by account index [sender, tx.Accounts[0], ...]
		addr, err := ac.AddressByIndex(accountIdx, sender)
		if err != nil {
			return err
		}

		// Ensure we did not already receive a non-empty LocalState
		// delta for this address, in case the caller passed us an
		// invalid EvalDelta
		_, ok := changes[addr]
		if ok {
			return &applyError{fmt.Sprintf("duplicate LocalState delta for %s", addr.String())}
		}

		// Zero-length LocalState deltas are not allowed. We should never produce
		// them from Eval.
		if len(delta) == 0 {
			return &applyError{fmt.Sprintf("got zero-length delta for %s, not allowed", addr.String())}
		}

		// Check that the local state delta isn't breaking any rules regarding
		// key/value lengths
		err = delta.Valid(&proto)
		if err != nil {
			return &applyError{fmt.Sprintf("cannot apply LocalState delta for %s: %v", addr.String(), err)}
		}

		record, err := balances.Get(addr, false)
		if err != nil {
			return err
		}

		localState, ok := record.AppLocalStates[appIdx]
		if !ok {
			return &applyError{fmt.Sprintf("cannot apply LocalState delta to %s: acct has not opted in to app %d", addr.String(), appIdx)}
		}

		// Clone LocalState so that we have a copy that is safe to modify
		localState = localState.Clone()

		// Allocate localState.KeyValue if necessary. We need to do
		// this now since an empty map will be read as nil from disk
		if localState.KeyValue == nil {
			localState.KeyValue = make(basics.TealKeyValue)
		}

		err = applyStateDelta(localState.KeyValue, delta)
		if err != nil {
			return err
		}

		// Make sure we haven't violated the LocalStateSchema
		err = localState.KeyValue.SatisfiesSchema(localState.Schema)
		if err != nil {
			return &applyError{fmt.Sprintf("LocalState for %s for app %d would use too much space: %v", addr.String(), appIdx, err)}
		}

		// Stage the change to be committed after all schema checks
		changes[addr] = localState
	}

	/*
	 * 3. Write any GlobalState changes back to cow. This should be correct
	 *    even if creator is in the local deltas, because the updated
	 *    fields are different.
	 */

	if len(evalDelta.GlobalDelta) > 0 {
		record, err := balances.Get(creator, false)
		if err != nil {
			return err
		}

		// Overwrite parameters for this appIdx with our cloned,
		// modified params
		record.AppParams = cloneAppParams(record.AppParams)
		record.AppParams[appIdx] = params

		err = balances.Put(record)
		if err != nil {
			return err
		}
	}

	/*
	 * 4. Write LocalState changes back to cow
	 */

	for addr, newLocalState := range changes {
		record, err := balances.Get(addr, false)
		if err != nil {
			return err
		}

		record.AppLocalStates = cloneAppLocalStates(record.AppLocalStates)
		record.AppLocalStates[appIdx] = newLocalState

		err = balances.Put(record)
		if err != nil {
			return err
		}
	}

	return nil
}

func checkPrograms(ac *transactions.ApplicationCallTxnFields, steva StateEvaluator, maxCost int) error {
	cost, err := steva.Check(ac.ApprovalProgram)
	if err != nil {
		return fmt.Errorf("check failed on ApprovalProgram: %v", err)
	}

	if cost > maxCost {
		return fmt.Errorf("ApprovalProgram too resource intensive. Cost is %d, max %d", cost, maxCost)
	}

	cost, err = steva.Check(ac.ClearStateProgram)
	if err != nil {
		return fmt.Errorf("check failed on ClearStateProgram: %v", err)
	}

	if cost > maxCost {
		return fmt.Errorf("ClearStateProgram too resource intensive. Cost is %d, max %d", cost, maxCost)
	}

	return nil
}

// DisallowObsoletePrograms when is set to true the installation of TEAL scripts with version <= 2 will be rejected.
const DisallowObsoletePrograms = false

func prepareProgram(bytecode []byte) ([]byte, error) {
	if len(bytecode) == 0 {
		return bytecode, nil
	}
	version := int(bytecode[0])
	if version == 3 {
		p, err := converter.NewProgram(bytecode)
		if err != nil {
			return nil, err
		}
		return p.ConvertTo(2)
	}
	if DisallowObsoletePrograms && version <= 2 {
		return nil, fmt.Errorf("TEAL version %d is obsolete", version)
	}
	return bytecode, nil
}

// createApplication writes a new AppParams entry and returns application ID
func createApplication(ac *transactions.ApplicationCallTxnFields, balances Balances, creator basics.Address, txnCounter uint64) (appIdx basics.AppIndex, err error) {
	// Fetch the creator's (sender's) balance record
	record, err := balances.Get(creator, false)
	if err != nil {
		return
	}

	// Make sure the creator isn't already at the app creation max
	maxAppsCreated := balances.ConsensusParams().MaxAppsCreated
	if len(record.AppParams) >= maxAppsCreated {
		err = fmt.Errorf("cannot create app for %s: max created apps per acct is %d", creator.String(), maxAppsCreated)
		return
	}

	convertedAp, err := prepareProgram(ac.ApprovalProgram)
	if err != nil {
		return 0, err
	}
	convertedCsp, err := prepareProgram(ac.ClearStateProgram)
	if err != nil {
		return 0, err
	}

	// Clone app params, so that we have a copy that is safe to modify
	record.AppParams = cloneAppParams(record.AppParams)

	// Allocate the new app params (+ 1 to match Assets Idx namespace)
	appIdx = basics.AppIndex(txnCounter + 1)
	record.AppParams[appIdx] = basics.AppParams{
		ApprovalProgram:   convertedAp,
		ClearStateProgram: convertedCsp,
		StateSchemas: basics.StateSchemas{
			LocalStateSchema:  ac.LocalStateSchema,
			GlobalStateSchema: ac.GlobalStateSchema,
		},
	}

	// Update the cached TotalStateSchema for this account, used
	// when computing MinBalance, since the creator has to store
	// the global state
	totalSchema := record.TotalAppSchema
	totalSchema = totalSchema.AddSchema(ac.GlobalStateSchema)
	record.TotalAppSchema = totalSchema

	// Tell the cow what app we created
	created := &basics.CreatableLocator{
		Creator: creator,
		Type:    basics.AppCreatable,
		Index:   basics.CreatableIndex(appIdx),
	}

	// Write back to the creator's balance record and continue
	err = balances.PutWithCreatable(record, created, nil)
	if err != nil {
		return 0, err
	}

	return
}

func applyClearState(ac *transactions.ApplicationCallTxnFields, balances Balances, sender basics.Address, appIdx basics.AppIndex, ad *transactions.ApplyData, steva StateEvaluator) error {
	// Fetch the application parameters, if they exist
	params, creator, exists, err := getAppParams(balances, appIdx)
	if err != nil {
		return err
	}

	record, err := balances.Get(sender, false)
	if err != nil {
		return err
	}

	// Ensure sender actually has LocalState allocated for this app.
	// Can't clear out if not currently opted in
	_, ok := record.AppLocalStates[appIdx]
	if !ok {
		return fmt.Errorf("cannot clear state for app %d: account %s is not currently opted in", appIdx, sender.String())
	}

	// If the application still exists...
	if exists {
		// Execute the ClearStateProgram before we've deleted the LocalState
		// for this account. If the ClearStateProgram does not fail, apply any
		// state deltas it generated.
		pass, evalDelta, err := steva.Eval(params.ClearStateProgram)
		if err == nil && pass {
			// Program execution may produce some GlobalState and LocalState
			// deltas. Apply them, provided they don't exceed the bounds set by
			// the GlobalStateSchema and LocalStateSchema. If they do exceed
			// those bounds, then don't fail, but also don't apply the changes.
			err = applyEvalDelta(ac, evalDelta, params, creator, sender, balances, appIdx)
			if err != nil && !isApplyError(err) {
				return err
			}

			// If we applied the changes, fill in applyData, so that consumers don't
			// have to implement a stateful TEAL interpreter to apply state changes
			if err == nil {
				ad.EvalDelta = evalDelta
			}
		} else {
			// Ignore errors and rejections from the ClearStateProgram
		}

		// Fetch the (potentially updated) sender record
		record, err = balances.Get(sender, false)
		if err != nil {
			return err
		}
	}

	// Update the TotalAppSchema used for MinBalance calculation,
	// since the sender no longer has to store LocalState
	totalSchema := record.TotalAppSchema
	localSchema := record.AppLocalStates[appIdx].Schema
	totalSchema = totalSchema.SubSchema(localSchema)
	record.TotalAppSchema = totalSchema

	// Deallocate the AppLocalState and finish
	record.AppLocalStates = cloneAppLocalStates(record.AppLocalStates)
	delete(record.AppLocalStates, appIdx)

	return balances.Put(record)
}

func applyOptIn(balances Balances, sender basics.Address, appIdx basics.AppIndex, params basics.AppParams) error {
	record, err := balances.Get(sender, false)
	if err != nil {
		return err
	}

	// If the user has already opted in, fail
	_, ok := record.AppLocalStates[appIdx]
	if ok {
		return fmt.Errorf("account %s has already opted in to app %d", sender.String(), appIdx)
	}

	// Make sure the user isn't already at the app opt-in max
	maxAppsOptedIn := balances.ConsensusParams().MaxAppsOptedIn
	if len(record.AppLocalStates) >= maxAppsOptedIn {
		return fmt.Errorf("cannot opt in app %d for %s: max opted-in apps per acct is %d", appIdx, sender.String(), maxAppsOptedIn)
	}

	// If the user hasn't opted in yet, allocate LocalState for the app
	record.AppLocalStates = cloneAppLocalStates(record.AppLocalStates)
	record.AppLocalStates[appIdx] = basics.AppLocalState{
		Schema: params.LocalStateSchema,
	}

	// Update the TotalAppSchema used for MinBalance calculation,
	// since the sender must now store LocalState
	totalSchema := record.TotalAppSchema
	totalSchema = totalSchema.AddSchema(params.LocalStateSchema)
	record.TotalAppSchema = totalSchema

	return balances.Put(record)
}

// ApplicationCall applies an ApplicationCall transaction using the Balances
// interface, recording key/value side effects inside of ApplyData.
func ApplicationCall(ac transactions.ApplicationCallTxnFields, header transactions.Header, balances Balances, ad *transactions.ApplyData, txnCounter uint64, steva StateEvaluator) (err error) {
	defer func() {
		// If we are returning a non-nil error, then don't return a
		// non-empty EvalDelta. Not required for correctness.
		if err != nil && ad != nil {
			ad.EvalDelta = basics.EvalDelta{}
		}
	}()

	// Keep track of the application ID we're working on
	appIdx := ac.ApplicationID

	// this is not the case in the current code but still probably better to check
	if ad == nil {
		err = fmt.Errorf("cannot use empty ApplyData")
		return
	}

	// Specifying an application ID of 0 indicates application creation
	if ac.ApplicationID == 0 {
		appIdx, err = createApplication(&ac, balances, header.Sender, txnCounter)
		if err != nil {
			return
		}
	}

	// Fetch the application parameters, if they exist
	params, creator, exists, err := getAppParams(balances, appIdx)
	if err != nil {
		return err
	}

	// Ensure that the only operation we can do is ClearState if the application
	// does not exist
	if !exists && ac.OnCompletion != transactions.ClearStateOC {
		return fmt.Errorf("only clearing out is supported for applications that do not exist")
	}

	// Initialize our TEAL evaluation context. Internally, this manages
	// access to balance records for Stateful TEAL programs as a thin
	// wrapper around Balances.
	//
	// Note that at this point in execution, the application might not exist
	// (e.g. if it was deleted). In that case, we will pass empty
	// params.StateSchemas below. This is OK because if the application is
	// deleted, we will never execute its programs.
	err = steva.InitLedger(balances, appIdx, params.StateSchemas)
	if err != nil {
		return err
	}

	// If this txn is going to set new programs (either for creation or
	// update), check that the programs are valid and not too expensive
	if ac.ApplicationID == 0 || ac.OnCompletion == transactions.UpdateApplicationOC {
		maxCost := balances.ConsensusParams().MaxAppProgramCost
		err = checkPrograms(&ac, steva, maxCost)
		if err != nil {
			return err
		}
	}

	// Clear out our LocalState. In this case, we don't execute the
	// ApprovalProgram, since clearing out is always allowed. We only
	// execute the ClearStateProgram, whose failures are ignored.
	if ac.OnCompletion == transactions.ClearStateOC {
		return applyClearState(&ac, balances, header.Sender, appIdx, ad, steva)
	}

	// If this is an OptIn transaction, ensure that the sender has
	// LocalState allocated prior to TEAL execution, so that it may be
	// initialized in the same transaction.
	if ac.OnCompletion == transactions.OptInOC {
		err = applyOptIn(balances, header.Sender, appIdx, params)
		if err != nil {
			return err
		}
	}

	// Execute the Approval program
	approved, evalDelta, err := steva.Eval(params.ApprovalProgram)
	if err != nil {
		return err
	}

	if !approved {
		return fmt.Errorf("transaction rejected by ApprovalProgram")
	}

	// Apply GlobalState and LocalState deltas, provided they don't exceed
	// the bounds set by the GlobalStateSchema and LocalStateSchema.
	// If they would exceed those bounds, then fail.
	err = applyEvalDelta(&ac, evalDelta, params, creator, header.Sender, balances, appIdx)
	if err != nil {
		return err
	}

	switch ac.OnCompletion {
	case transactions.NoOpOC:
		// Nothing to do

	case transactions.OptInOC:
		// Handled above

	case transactions.CloseOutOC:
		// Closing out of the application. Fetch the sender's balance record
		record, err := balances.Get(header.Sender, false)
		if err != nil {
			return err
		}

		// If they haven't opted in, that's an error
		localState, ok := record.AppLocalStates[appIdx]
		if !ok {
			return fmt.Errorf("account %s is not opted in to app %d", header.Sender.String(), appIdx)
		}

		// Update the TotalAppSchema used for MinBalance calculation,
		// since the sender no longer has to store LocalState
		totalSchema := record.TotalAppSchema
		totalSchema = totalSchema.SubSchema(localState.Schema)
		record.TotalAppSchema = totalSchema

		// Delete the local state
		record.AppLocalStates = cloneAppLocalStates(record.AppLocalStates)
		delete(record.AppLocalStates, appIdx)

		err = balances.Put(record)
		if err != nil {
			return err
		}

	case transactions.DeleteApplicationOC:
		// Deleting the application. Fetch the creator's balance record
		record, err := balances.Get(creator, false)
		if err != nil {
			return err
		}

		// Update the TotalAppSchema used for MinBalance calculation,
		// since the creator no longer has to store the GlobalState
		totalSchema := record.TotalAppSchema
		globalSchema := record.AppParams[appIdx].GlobalStateSchema
		totalSchema = totalSchema.SubSchema(globalSchema)
		record.TotalAppSchema = totalSchema

		// Delete the AppParams
		record.AppParams = cloneAppParams(record.AppParams)
		delete(record.AppParams, appIdx)

		// Tell the cow what app we deleted
		deleted := &basics.CreatableLocator{
			Creator: creator,
			Type:    basics.AppCreatable,
			Index:   basics.CreatableIndex(appIdx),
		}

		// Write back to cow
		err = balances.PutWithCreatable(record, nil, deleted)
		if err != nil {
			return err
		}

	case transactions.UpdateApplicationOC:
		// Updating the application. Fetch the creator's balance record
		record, err := balances.Get(creator, false)
		if err != nil {
			return err
		}

		// Fill in the new programs
		record.AppParams = cloneAppParams(record.AppParams)
		params := record.AppParams[appIdx]
		params.ApprovalProgram = ac.ApprovalProgram
		params.ClearStateProgram = ac.ClearStateProgram

		record.AppParams[appIdx] = params
		err = balances.Put(record)
		if err != nil {
			return err
		}

	default:
		return fmt.Errorf("invalid application action")
	}

	// Fill in applyData, so that consumers don't have to implement a
	// stateful TEAL interpreter to apply state changes
	ad.EvalDelta = evalDelta

	return nil
}

// Copyright 2018 The Nakama Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/dop251/goja"
	"github.com/gofrs/uuid"
	"github.com/golang/protobuf/jsonpb"
	"github.com/heroiclabs/nakama-common/rtapi"
	"github.com/heroiclabs/nakama/v2/social"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

type RuntimeJavascriptMatchCore struct {
	logger        *zap.Logger
	matchRegistry MatchRegistry
	router        MessageRouter

	deferMessageFn RuntimeMatchDeferMessageFunction
	presenceList   *MatchPresenceList

	id      uuid.UUID
	node    string
	stopped *atomic.Bool
	idStr   string
	stream  PresenceStream
	label   *atomic.String

	vm            *goja.Runtime
	initFn        goja.Callable
	joinAttemptFn goja.Callable
	joinFn        goja.Callable
	leaveFn       goja.Callable
	loopFn        goja.Callable
	terminateFn   goja.Callable
	ctx           *goja.Object
	dispatcher    goja.Value
	nakamaModule  goja.Value
	loggerModule  goja.Value

	// ctxCancelFn context.CancelFunc
}

func NewRuntimeJavascriptMatchCore(logger *zap.Logger, db *sql.DB, jsonpbMarshaler *jsonpb.Marshaler, jsonpbUnmarshaler *jsonpb.Unmarshaler, config Config, socialClient *social.Client, leaderboardCache LeaderboardCache, rankCache LeaderboardRankCache, leaderboardScheduler LeaderboardScheduler, sessionRegistry SessionRegistry, matchRegistry MatchRegistry, tracker Tracker, streamManager StreamManager, router MessageRouter, goMatchCreateFn RuntimeMatchCreateFunction, eventFn RuntimeEventCustomFunction, id uuid.UUID, node string, stopped *atomic.Bool, matchHandlers *jsMatchHandlers) (RuntimeMatchCore, error) {
	runtime := goja.New()

	jsLogger := NewJsLogger(logger)
	jsLoggerValue := runtime.ToValue(jsLogger.Constructor(runtime))
	jsLoggerInst, err := runtime.New(jsLoggerValue)
	if err != nil {
		logger.Fatal("Failed to initialize Javascript runtime", zap.Error(err))
	}

	nakamaModule := NewRuntimeJavascriptNakamaModule(logger, db, jsonpbMarshaler, jsonpbUnmarshaler, config, socialClient, leaderboardCache, rankCache, leaderboardScheduler, sessionRegistry, matchRegistry, tracker, streamManager, router, eventFn, goMatchCreateFn)
	nk := runtime.ToValue(nakamaModule.Constructor(runtime))
	nkInst, err := runtime.New(nk)
	if err != nil {
		logger.Fatal("Failed to initialize Javascript runtime", zap.Error(err))
	}

	ctx := NewRuntimeJsInitContext(runtime, node, config.GetRuntime().Environment)
	ctx.Set(__RUNTIME_JAVASCRIPT_CTX_MODE, RuntimeExecutionModeMatch)
	ctx.Set(__RUNTIME_JAVASCRIPT_CTX_MATCH_ID, fmt.Sprintf("%v.%v", id.String(), node))
	ctx.Set(__RUNTIME_JAVASCRIPT_CTX_MATCH_NODE, node)

	// goCtx, ctxCancelFn := context.WithCancel(context.Background())
	// vm.SetContext(goCtx)

	core := &RuntimeJavascriptMatchCore{
		logger:         logger,
		matchRegistry:  matchRegistry,
		router:         router,

		id:             id,
		node:           node,
		stopped:        stopped,
		idStr:          fmt.Sprintf("%v.%v", id.String(), node),
		stream:         PresenceStream{
			Mode:    StreamModeMatchAuthoritative,
			Subject: id,
			Label:   node,
		},
		label:          atomic.NewString(""),
		vm:             runtime,
		initFn:         matchHandlers.initFn,
		joinAttemptFn:  matchHandlers.joinAttemptFn,
		joinFn:         matchHandlers.joinFn,
		leaveFn:        matchHandlers.leaveFn,
		loopFn:         matchHandlers.loopFn,
		terminateFn:    matchHandlers.terminateFn,
		ctx:            ctx,

		loggerModule: jsLoggerInst,
		nakamaModule: nkInst,
		// ctxCancelFn: ctxCancelFn,
	}

	dispatcher := runtime.ToValue(
		func(call goja.ConstructorCall) *goja.Object {
			call.This.Set("broadcastMessage", core.broadcastMessage(runtime))
			call.This.Set("broadcastMessageDeferred", core.broadcastMessageDeferred(runtime))
			call.This.Set("matchKick", core.matchKick(runtime))
			call.This.Set("matchLabelUpdate", core.matchLabelUpdate(runtime))

			freeze(call.This)

			return nil
		},
	)

	dispatcherInst, err := runtime.New(dispatcher)
	if err != nil {
		logger.Fatal("Failed to initialize Javascript runtime", zap.Error(err))
	}
	core.dispatcher = dispatcherInst

	return core, nil
}

func (rm *RuntimeJavascriptMatchCore) MatchInit(presenceList *MatchPresenceList, deferMessageFn RuntimeMatchDeferMessageFunction, params map[string]interface{}) (interface{}, int, error) {
	args := []goja.Value{rm.ctx, rm.loggerModule, rm.nakamaModule, rm.vm.ToValue(params)}

	retVal, err := rm.initFn(goja.Null(), args...)
	if err != nil {
		return nil, 0, err
	}

	retMap, ok := retVal.Export().(map[string]interface{})
	if !ok {
		return nil, 0, errors.New("match_init is expected to return an object with 'state', 'tick_rate' and 'label' properties")
	}
	tickRateRet, ok := retMap["tick_rate"]
	if !ok {
		return nil, 0, errors.New("match_init return value has no 'tick_rate' property")
	}
	rate, ok := tickRateRet.(int64)
	if !ok {
		return nil, 0, errors.New("match_init 'tick_rate' must be a number between 1 and 30")
	}
	if rate < 1 || rate > 30 {
		return nil, 0, errors.New("match_init 'tick_rate' must be a number between 1 and 30")
	}

	var label string
	labelRet, ok := retMap["label"]
	if ok {
		label, ok = labelRet.(string)
		if !ok {
			return nil, 0, errors.New("match_init 'label' value must be a string")
		}
	}

	state, ok := retMap["state"]
	if !ok {
		return nil, 0, errors.New("match_init is expected to return an object with a 'state' property")
	}

	rm.ctx.Set(__RUNTIME_JAVASCRIPT_CTX_MATCH_LABEL, label)
	rm.ctx.Set(__RUNTIME_JAVASCRIPT_CTX_MATCH_TICK_RATE, rate)

	rm.deferMessageFn = deferMessageFn
	rm.presenceList = presenceList

	return state, int(rate), nil
}

func (rm *RuntimeJavascriptMatchCore) MatchJoinAttempt(tick int64, state interface{}, userID, sessionID uuid.UUID, username string, sessionExpiry int64, vars map[string]string, clientIP, clientPort, node string, metadata map[string]string) (interface{}, bool, string, error) {
	// Setup presence
	presenceObj := rm.vm.NewObject()
	presenceObj.Set("user_id", userID.String())
	presenceObj.Set("session_id", sessionID.String())
	presenceObj.Set("username", username)
	presenceObj.Set("node", node)

	// Setup ctx
	ctxObj := rm.vm.NewObject()
	for _, key := range rm.ctx.Keys() {
		ctxObj.Set(key, rm.ctx.Get(key))
	}
	ctxObj.Set(__RUNTIME_JAVASCRIPT_CTX_USER_ID, userID.String())
	ctxObj.Set(__RUNTIME_JAVASCRIPT_CTX_USERNAME, username)
	if vars != nil {
		ctxObj.Set(__RUNTIME_JAVASCRIPT_CTX_VARS, vars)
	}
	ctxObj.Set(__RUNTIME_JAVASCRIPT_CTX_USER_SESSION_EXP, sessionExpiry)
	ctxObj.Set(__RUNTIME_JAVASCRIPT_CTX_SESSION_ID, sessionID.String())
	if clientIP != "" {
		ctxObj.Set(__RUNTIME_JAVASCRIPT_CTX_CLIENT_IP, clientIP)
	}
	if clientPort != "" {
		ctxObj.Set(__RUNTIME_JAVASCRIPT_CTX_CLIENT_PORT, clientPort)
	}

	args := []goja.Value{ctxObj, rm.loggerModule, rm.nakamaModule, rm.dispatcher, rm.vm.ToValue(tick), rm.vm.ToValue(state), presenceObj, rm.vm.ToValue(metadata)}
	retVal, err := rm.joinAttemptFn(goja.Null(), args...)
	if err != nil {
		return nil, false, "", err
	}

	retMap, ok := retVal.Export().(map[string]interface{})
	if !ok {
		return nil, false, "", errors.New("match_join_attempt is expected to return an object with 'state' and 'accept' properties")
	}

	allowRet, ok := retMap["accept"]
  if !ok {
		return nil, false, "", errors.New("match_join_attempt return value has an 'accept' property")
	}
	allow, ok := allowRet.(bool)
	if !ok {
		return nil, false, "", errors.New("match_join_attempt 'accept' property must be a boolean")
	}

	var rejectMsg string
	if allow == false {
		rejectMsgRet, ok := retMap["reject_msg"]
		if ok {
			rejectMsg, ok = rejectMsgRet.(string)
			if !ok {
				return nil, false, "", errors.New("match_join_attempt 'reject_msg' property must be a string")
			}
		}
	}

	newState, ok := retMap["state"]
	if !ok {
		return nil, false, "", errors.New("match_join_attempt is expected to return an object with 'state' property")
	}

	return newState, allow, rejectMsg, nil
}

func (rm *RuntimeJavascriptMatchCore) MatchJoin(tick int64, state interface{}, joins []*MatchPresence) (interface{}, error) {
	presences := make([]interface{}, 0, len(joins))
	for _, p := range joins {
		presenceObj := rm.vm.NewObject()
		presenceObj.Set("user_id", p.UserID.String())
		presenceObj.Set("session_id", p.SessionID.String())
		presenceObj.Set("username", p.Username)
		presenceObj.Set("node", p.Node)

		presences = append(presences, presenceObj)
	}

	args := []goja.Value{rm.ctx, rm.loggerModule, rm.nakamaModule, rm.dispatcher, rm.vm.ToValue(tick), rm.vm.ToValue(state), rm.vm.ToValue(presences)}
	retVal, err := rm.joinFn(goja.Null(), args...)
	if err != nil {
		return nil, err
	}

	retMap, ok := retVal.Export().(map[string]interface{})
	if !ok {
		return nil, errors.New("match_join is expected to return an object with 'state' property")
	}

	newState, ok := retMap["state"]
	if !ok {
		return nil, errors.New("match_join is expected to return an object with 'state' property")
	}

	return newState, nil
}

func (rm *RuntimeJavascriptMatchCore) MatchLeave(tick int64, state interface{}, leaves []*MatchPresence) (interface{}, error) {
	presences := make([]interface{}, 0, len(leaves))
	for _, p := range leaves {
		presenceObj := rm.vm.NewObject()
		presenceObj.Set("user_id", p.UserID.String())
		presenceObj.Set("session_id", p.SessionID.String())
		presenceObj.Set("username", p.Username)
		presenceObj.Set("node", p.Node)

		presences = append(presences, presenceObj)
	}

	args := []goja.Value{rm.ctx, rm.loggerModule, rm.nakamaModule, rm.dispatcher, rm.vm.ToValue(tick), rm.vm.ToValue(state), rm.vm.ToValue(presences)}
	retVal, err := rm.leaveFn(goja.Null(), args...)
	if err != nil {
		return nil, err
	}

	retMap, ok := retVal.Export().(map[string]interface{})
	if !ok {
		return nil, errors.New("match_leave is expected to return an object with 'state' property")
	}

	newState, ok := retMap["state"]
	if !ok {
		return nil, errors.New("match_leave is expected to return an object with 'state' property")
	}

	return newState, nil
}

func (rm *RuntimeJavascriptMatchCore) MatchLoop(tick int64, state interface{}, inputCh <-chan *MatchDataMessage) (interface{}, error) {
	// Drain the input queue into a Lua table.
	size := len(inputCh)
	inputs := make([]interface{}, 0, size)
	for i := 1; i <= size; i++ {
		msg := <- inputCh

		presenceObj := rm.vm.NewObject()
		presenceObj.Set("user_id", msg.UserID.String())
		presenceObj.Set("session_id", msg.SessionID.String())
		presenceObj.Set("username", msg.Username)
		presenceObj.Set("node", msg.Node)

		msgObj := rm.vm.NewObject()
		msgObj.Set("sender", presenceObj)
		msgObj.Set("op_code", msg.OpCode)
		if msg.Data != nil {
			msgObj.Set("data", string(msg.Data))
		} else {
			msgObj.Set("data", goja.Null())
		}
		msgObj.Set("reliable", msg.Reliable)
		msgObj.Set("receive_time_ms", msg.ReceiveTime)

		inputs = append(inputs, msgObj)
	}

	args := []goja.Value{rm.ctx, rm.loggerModule, rm.nakamaModule, rm.dispatcher, rm.vm.ToValue(tick), rm.vm.ToValue(state), rm.vm.ToValue(inputs)}
	retVal, err := rm.loopFn(goja.Null(), args...)
	if err != nil {
		return nil, err
	}

	if goja.IsNull(retVal) || goja.IsUndefined(retVal) {
		return nil, nil
	}

	retMap, ok := retVal.Export().(map[string]interface{})
	if !ok {
		return nil, errors.New("match_loop is expected to return an object with 'state' property")
	}

	newState, ok := retMap["state"]
	if !ok {
		return nil, errors.New("match_loop is expected to return an object with 'state' property")
	}

	return newState, nil
}

func (rm *RuntimeJavascriptMatchCore) MatchTerminate(tick int64, state interface{}, graceSeconds int) (interface{}, error) {
	args := []goja.Value{rm.ctx, rm.loggerModule, rm.nakamaModule, rm.dispatcher, rm.vm.ToValue(tick), rm.vm.ToValue(state), rm.vm.ToValue(graceSeconds)}
	retVal, err := rm.terminateFn(goja.Null(), args...)
	if err != nil {
		return nil, err
	}

	retMap, ok := retVal.Export().(map[string]interface{})
	if !ok {
		return nil, errors.New("match_terminate is expected to return an object with 'state' property")
	}

	newState, ok := retMap["state"]
	if !ok {
		return nil, errors.New("match_terminate is expected to return an object with 'state' property")
	}

	return newState, nil
}

func (rm *RuntimeJavascriptMatchCore) Label() string {
	return rm.label.Load()
}

func (rm *RuntimeJavascriptMatchCore) Cancel() {
  // TODO: implement cancel
}

func (rm *RuntimeJavascriptMatchCore) broadcastMessage(r *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(f goja.FunctionCall) goja.Value {
		if rm.stopped.Load() {
			panic(r.ToValue("match stopped"))
		}

		presenceIDs, msg, reliable := rm.validateBroadcast(r, f)
		if len(presenceIDs) != 0 {
			rm.router.SendToPresenceIDs(rm.logger, presenceIDs, msg, reliable)
		}

		return goja.Undefined()
	}
}

func (rm *RuntimeJavascriptMatchCore) broadcastMessageDeferred(r *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(f goja.FunctionCall) goja.Value {
		if rm.stopped.Load() {
			panic(r.ToValue("match stopped"))
		}

		presenceIDs, msg, reliable := rm.validateBroadcast(r, f)
		if len(presenceIDs) != 0 {
			if err := rm.deferMessageFn(&DeferredMessage{
				PresenceIDs: presenceIDs,
				Envelope:    msg,
				Reliable:    reliable,
			}); err != nil {
				panic(r.ToValue(fmt.Sprintf("error deferring message broadcast: %v", err)))
			}
		}

		return goja.Undefined()
	}
}

func(rm *RuntimeJavascriptMatchCore) validateBroadcast(r *goja.Runtime, f goja.FunctionCall) ([]*PresenceID, *rtapi.Envelope, bool) {
	opCode := getJsInt(r, f.Argument(0))

	var dataBytes []byte
	data := f.Argument(1)
	if !goja.IsUndefined(data) && !goja.IsNull(data) {
		dataStr, ok := data.Export().(string)
		if !ok {
			panic(r.NewTypeError("expects data to be a string or nil"))
		}
		dataBytes = []byte(dataStr)
	}

	filter := f.Argument(2)
	var presenceIDs []*PresenceID
	if !goja.IsUndefined(filter) && !goja.IsNull(filter) {
		filterSlice, ok := filter.Export().([]interface{})
		if !ok {
			panic(r.NewTypeError("expects an array of presences or nil"))
		}

		presenceIDs = make([]*PresenceID, 0, len(filterSlice))
		for _, p := range filterSlice {
			pMap, ok := p.(map[string]interface{})
			if !ok {
				panic(r.NewTypeError("expects a valid set of presences"))
			}

			presenceID := &PresenceID{}

			sidVal, _ := pMap["session_id"]
			if sidVal == nil {
				panic(r.NewTypeError("presence is expected to contain a 'session_id'"))
			}
			sidStr, ok := sidVal.(string)
			if !ok {
				panic(r.NewTypeError("expects a 'session_id' string"))
			}
			sid, err := uuid.FromString(sidStr)
			if err != nil {
				panic(r.NewTypeError("expects a valid 'session_id'"))
			}

			nodeVal, _ := pMap["node_id"]
			if nodeVal == nil {
				panic(r.NewTypeError("expects presence to contain a 'node_id'"))
			}
			node, ok := nodeVal.(string)
			if !ok {
				panic(r.NewTypeError("expects a 'node_id' string"))
			}

			presenceID.SessionID = sid
			presenceID.Node = node

			presenceIDs = append(presenceIDs, presenceID)
		}
	}

	if presenceIDs != nil && len(presenceIDs) == 0 {
		// Filter is empty, there are no requested message targets.
		return nil, nil, false
	}

	sender := f.Argument(3)
	var presence *rtapi.UserPresence

	if !goja.IsUndefined(sender) && !goja.IsNull(sender) {
		presence = &rtapi.UserPresence{}

		senderMap, ok := sender.Export().(map[string]interface{})
		if !ok {
			panic(r.NewTypeError("expects sender to be an object"))
		}
		userIdVal, _ := senderMap["user_id"]
		if userIdVal == nil {
			panic(r.NewTypeError("expects presence to contain 'user_id'"))
		}
		userIDStr, ok := userIdVal.(string)
		if !ok {
			panic(r.NewTypeError("expects presence to contain 'user_id' string"))
		}
		_, err := uuid.FromString(userIDStr)
		if err != nil {
			panic(r.NewTypeError("expects presence to contain valid user_id"))
		}
		presence.UserId = userIDStr

		sidVal, _ := senderMap["session_id"]
		if sidVal == nil {
			panic(r.NewTypeError("presence is expected to contain a 'session_id'"))
		}
		sidStr, ok := sidVal.(string)
		if !ok {
			panic(r.NewTypeError("expects a 'session_id' string"))
		}
		_, err = uuid.FromString(sidStr)
		if err != nil {
			panic(r.NewTypeError("expects a valid 'session_id'"))
		}
		presence.SessionId = sidStr

		usernameVal, _ := senderMap["username"]
		if usernameVal == nil {
			panic(r.NewTypeError("presence is expected to contain a 'username'"))
		}
		username, ok := sidVal.(string)
		if !ok {
			panic(r.NewTypeError("expects a 'username' string"))
		}
		presence.Username = username
	}

	if presenceIDs != nil {
		// Ensure specific presences actually exist to prevent sending bogus messages to arbitrary users.
		if len(presenceIDs) == 1 && filter != nil {
			if !rm.presenceList.Contains(presenceIDs[0]) {
				return nil, nil, false
			}
		} else {
			actualPresenceIDs := rm.presenceList.ListPresenceIDs()
			for i := 0; i < len(presenceIDs); i++ {
				found := false
				presenceID := presenceIDs[i]
				for j := 0; j < len(actualPresenceIDs); j++ {
					if actual := actualPresenceIDs[j]; presenceID.SessionID == actual.SessionID && presenceID.Node == actual.Node {
						// If it matches, drop it.
						actualPresenceIDs[j] = actualPresenceIDs[len(actualPresenceIDs)-1]
						actualPresenceIDs = actualPresenceIDs[:len(actualPresenceIDs)-1]
						found = true
						break
					}
				}
				if !found {
					// If this presence wasn't in the filters, it's not needed.
					presenceIDs[i] = presenceIDs[len(presenceIDs)-1]
					presenceIDs = presenceIDs[:len(presenceIDs)-1]
					i--
				}
			}
			if len(presenceIDs) == 0 {
				// None of the target presenceIDs existed in the list of match members.
				return nil, nil, false
			}
		}
	}

	reliable := true
	reliableVal := f.Argument(4)
	if !goja.IsUndefined(reliableVal) && !goja.IsNull(reliableVal) {
		reliable = getJsBool(r, reliableVal)
	}

	msg := &rtapi.Envelope{Message: &rtapi.Envelope_MatchData{MatchData: &rtapi.MatchData{
		MatchId:  rm.idStr,
		Presence: presence,
		OpCode:   opCode,
		Data:     dataBytes,
		Reliable: reliable,
	}}}

	if presenceIDs == nil {
		presenceIDs = rm.presenceList.ListPresenceIDs()
	}

	return presenceIDs, msg, reliable
}

func (rm *RuntimeJavascriptMatchCore) matchKick(r *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(f goja.FunctionCall) goja.Value {
		if rm.stopped.Load() {
			panic(r.ToValue("match stopped"))
		}

		input := f.Argument(0)
		if goja.IsUndefined(input) || goja.IsNull(input) {
			return goja.Undefined()
		}

		presencesSlice, ok := input.Export().([]interface{})
		if !ok {
			panic(r.ToValue("expects an array of presence objects"))
		}

		presences := make([]*MatchPresence, 0, len(presencesSlice))
		for _, p := range presencesSlice {
			pMap, ok := p.(map[string]interface{})
			if !ok {
				panic(r.NewTypeError("expects a valid set of presences"))
			}

			presence := &MatchPresence{}
			userIdVal, _ := pMap["user_id"]
			if userIdVal == nil {
				panic(r.NewTypeError("expects presence to contain 'user_id'"))
			}
			userIDStr, ok := userIdVal.(string)
			if !ok {
				panic(r.NewTypeError("expects presence to contain 'user_id' string"))
			}
			uid, err := uuid.FromString(userIDStr)
			if err != nil {
				panic(r.NewTypeError("expects presence to contain valid user_id"))
			}
			presence.UserID = uid

			sidVal, _ := pMap["session_id"]
			if sidVal == nil {
				panic(r.NewTypeError("presence is expected to contain a 'session_id'"))
			}
			sidStr, ok := sidVal.(string)
			if !ok {
				panic(r.NewTypeError("expects a 'session_id' string"))
			}
			sid, err := uuid.FromString(sidStr)
			if err != nil {
				panic(r.NewTypeError("expects a valid 'session_id'"))
			}
			presence.SessionID = sid

			nodeVal, _ := pMap["node_id"]
			if nodeVal == nil {
				panic(r.NewTypeError("expects presence to contain a 'node_id'"))
			}
			node, ok := nodeVal.(string)
			if !ok {
				panic(r.NewTypeError("expects a 'node_id' string"))
			}
			presence.Node = node

			presences = append(presences, presence)
		}

		rm.matchRegistry.Kick(rm.stream, presences)

		return goja.Undefined()
	}
}

func (rm *RuntimeJavascriptMatchCore) matchLabelUpdate(r *goja.Runtime) func(goja.FunctionCall) goja.Value {
	return func(f goja.FunctionCall) goja.Value {
		if rm.stopped.Load() {
			panic(r.ToValue("match stopped"))
		}

		input := getJsString(r, f.Argument(0))

		if err := rm.matchRegistry.UpdateMatchLabel(rm.id, input); err != nil {
			panic(r.ToValue(fmt.Sprintf("error updating match label: %v", err.Error())))
		}
		rm.label.Store(input)

		// This must be executed from inside a match call so safe to update here.
		rm.ctx.Set(__RUNTIME_JAVASCRIPT_CTX_MATCH_LABEL, input)

		return goja.Undefined()
	}
}

// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/pingcap/errors"
	bf "github.com/pingcap/tidb-tools/pkg/binlog-filter"
	"github.com/siddontang/go/sync2"
	"go.etcd.io/etcd/clientv3"
	"go.uber.org/zap"

	"github.com/pingcap/dm/dm/config"
	"github.com/pingcap/dm/dm/pb"
	"github.com/pingcap/dm/pkg/binlog"
	"github.com/pingcap/dm/pkg/etcdutil"
	"github.com/pingcap/dm/pkg/ha"
	"github.com/pingcap/dm/pkg/log"
	"github.com/pingcap/dm/pkg/terror"
	"github.com/pingcap/dm/pkg/utils"
	"github.com/pingcap/dm/relay/purger"
)

var (
	closedFalse int32
	closedTrue  int32 = 1
)

// Worker manages sub tasks and process units for data migration
type Worker struct {
	// ensure no other operation can be done when closing (we can use `WatGroup`/`Context` to archive this)
	sync.RWMutex

	wg     sync.WaitGroup
	closed sync2.AtomicInt32

	// context created when Worker created, and canceled when closing
	ctx    context.Context
	cancel context.CancelFunc

	cfg *config.SourceConfig
	l   log.Logger

	subTaskHolder *subTaskHolder

	relayHolder RelayHolder
	relayPurger purger.Purger

	taskStatusChecker TaskStatusChecker

	etcdClient *clientv3.Client

	name string
}

// NewWorker creates a new Worker. The functionality of relay and subtask is disabled by default, need call EnableRelay
// and EnableSubtask later
func NewWorker(cfg *config.SourceConfig, etcdClient *clientv3.Client, name string) (w *Worker, err error) {
	w = &Worker{
		cfg:           cfg,
		subTaskHolder: newSubTaskHolder(),
		l:             log.With(zap.String("component", "worker controller")),
		etcdClient:    etcdClient,
		name:          name,
	}
	// keep running until canceled in `Close`.
	w.ctx, w.cancel = context.WithCancel(context.Background())
	w.closed.Set(closedTrue)

	defer func(w2 *Worker) {
		if err != nil { // when err != nil, `w` will become nil in this func, so we pass `w` in defer.
			// release resources, NOTE: we need to refactor New/Init/Start/Close for components later.
			w2.cancel()
			w2.subTaskHolder.closeAllSubTasks()
		}
	}(w)

	// initial task status checker
	if w.cfg.Checker.CheckEnable {
		tsc := NewTaskStatusChecker(w.cfg.Checker, w)
		err = tsc.Init()
		if err != nil {
			return nil, err
		}
		w.taskStatusChecker = tsc
	}

	InitConditionHub(w)

	w.l.Info("initialized", zap.Stringer("cfg", cfg))

	return w, nil
}

// Start starts working
func (w *Worker) Start() {
	// start task status checker
	if w.cfg.Checker.CheckEnable {
		w.taskStatusChecker.Start()
	}

	w.wg.Add(1)
	defer w.wg.Done()

	w.l.Info("start running")

	ticker := time.NewTicker(5 * time.Second)
	w.closed.Set(closedFalse)
	defer ticker.Stop()
	for {
		select {
		case <-w.ctx.Done():
			w.l.Info("status print process exits!")
			return
		case <-ticker.C:
			w.l.Debug("runtime status", zap.String("status", w.StatusJSON(w.ctx, "")))
		}
	}
}

// Close stops working and releases resources
func (w *Worker) Close() {
	if w.closed.Get() == closedTrue {
		w.l.Warn("already closed")
		return
	}

	// cancel status output ticker and wait for return
	w.cancel()
	w.wg.Wait()

	w.Lock()
	defer w.Unlock()

	// close all sub tasks
	w.subTaskHolder.closeAllSubTasks()

	if w.relayHolder != nil {
		// close relay
		w.relayHolder.Close()
	}

	if w.relayPurger != nil {
		// close purger
		w.relayPurger.Close()
	}

	// close task status checker
	if w.cfg.Checker.CheckEnable {
		w.taskStatusChecker.Close()
	}

	w.closed.Set(closedTrue)
	w.l.Info("Stop worker")
}

// EnableRelay enables the functionality of start/watch/handle relay
func (w *Worker) EnableRelay() error {
	// 1. adjust relay starting position, to the earliest of subtasks
	_, subTaskCfgs, _, err := w.fetchSubTasksAndAdjust()
	if err != nil {
		return err
	}

	dctx, dcancel := context.WithTimeout(w.etcdClient.Ctx(), time.Duration(len(subTaskCfgs))*3*time.Second)
	defer dcancel()
	minLoc, err1 := getMinLocInAllSubTasks(dctx, subTaskCfgs)
	if err1 != nil {
		return err1
	}

	if minLoc != nil {
		log.L().Info("get min location in all subtasks", zap.Stringer("location", *minLoc))
		w.cfg.RelayBinLogName = binlog.AdjustPosition(minLoc.Position).Name
		w.cfg.RelayBinlogGTID = minLoc.GTIDSetStr()
		// set UUIDSuffix when bound to a source
		w.cfg.UUIDSuffix, err = binlog.ExtractSuffix(minLoc.Position.Name)
		if err != nil {
			return err
		}
	} else {
		// set UUIDSuffix even not checkpoint exist
		// so we will still remove relay dir
		w.cfg.UUIDSuffix = binlog.MinUUIDSuffix
	}

	// 2. initial relay holder, the cfg's password need decrypt
	w.relayHolder = NewRelayHolder(w.cfg)
	relayPurger, err := w.relayHolder.Init([]purger.PurgeInterceptor{
		w,
	})
	if err != nil {
		return err
	}
	w.relayPurger = relayPurger

	// 3. get relay stage from etcd and check if need starting
	// we get the newest relay stages directly which will omit the relay stage PUT/DELETE event
	// because triggering these events is useless now
	relayStage, revRelay, err := ha.GetRelayStage(w.etcdClient, w.cfg.SourceID)
	if err != nil {
		// TODO: need retry
		return err
	}
	startImmediately := !relayStage.IsDeleted && relayStage.Expect == pb.Stage_Running
	if startImmediately {
		log.L().Info("relay is started")
		w.relayHolder.Start()
		w.relayPurger.Start()
	}

	// 4. watch relay stage
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		// TODO: handle fatal error from observeRelayStage
		//nolint:errcheck
		w.observeRelayStage(w.ctx, w.etcdClient, revRelay)
	}()
	return nil
}

// EnableHandleSubtasks enables the functionality of start/watch/handle subtasks
func (w *Worker) EnableHandleSubtasks() error {
	subTaskStages, subTaskCfgM, revSubTask, err := w.fetchSubTasksAndAdjust()
	if err != nil {
		return err
	}

	log.L().Info("starting to handle mysql source", zap.String("sourceCfg", w.cfg.String()), zap.Any("subTasks", subTaskCfgM))

	for _, subTaskCfg := range subTaskCfgM {
		expectStage := subTaskStages[subTaskCfg.Name]
		if expectStage.IsDeleted {
			continue
		}
		log.L().Info("start to create subtask", zap.String("sourceID", subTaskCfg.SourceID), zap.String("task", subTaskCfg.Name))
		// for range of a map will use a same value-address, so we'd better not pass value-address to other function
		clone := subTaskCfg
		if err := w.StartSubTask(&clone, expectStage.Expect); err != nil {
			return err
		}
	}

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		// TODO: handle fatal error from observeSubtaskStage
		//nolint:errcheck
		w.observeSubtaskStage(w.ctx, w.etcdClient, revSubTask)
	}()

	return nil
}

// fetchSubTasksAndAdjust gets source's subtask stages and configs, adjust some values by worker's config and status
// source **must not be empty**
// return map{task name -> subtask stage}, map{task name -> subtask config}, revision, error.
func (w *Worker) fetchSubTasksAndAdjust() (map[string]ha.Stage, map[string]config.SubTaskConfig, int64, error) {
	// we get the newest subtask stages directly which will omit the subtask stage PUT/DELETE event
	// because triggering these events is useless now
	subTaskStages, subTaskCfgM, revSubTask, err := ha.GetSubTaskStageConfig(w.etcdClient, w.cfg.SourceID)
	if err != nil {
		return nil, nil, 0, err
	}

	if err = copyConfigFromSourceForEach(subTaskCfgM, w.cfg); err != nil {
		return nil, nil, 0, err
	}
	return subTaskStages, subTaskCfgM, revSubTask, nil
}

// StartSubTask creates a sub task an run it
func (w *Worker) StartSubTask(cfg *config.SubTaskConfig, expectStage pb.Stage) error {
	w.Lock()
	defer w.Unlock()

	// copy some config item from dm-worker's source config
	err := copyConfigFromSource(cfg, w.cfg)
	if err != nil {
		return err
	}

	// directly put cfg into subTaskHolder
	// the unique of subtask should be assured by etcd
	st := NewSubTask(cfg, w.etcdClient)
	w.subTaskHolder.recordSubTask(st)
	if w.closed.Get() == closedTrue {
		st.fail(terror.ErrWorkerAlreadyClosed.Generate())
		return nil
	}

	cfg2, err := cfg.DecryptPassword()
	if err != nil {
		st.fail(errors.Annotate(err, "start sub task"))
		return nil
	}
	st.cfg = cfg2

	if w.relayPurger != nil && w.relayPurger.Purging() {
		// TODO: retry until purged finished
		st.fail(terror.ErrWorkerRelayIsPurging.Generate(cfg.Name))
		return nil
	}

	w.l.Info("subtask created", zap.Stringer("config", cfg2))
	st.Run(expectStage)
	return nil
}

// UpdateSubTask update config for a sub task
func (w *Worker) UpdateSubTask(cfg *config.SubTaskConfig) error {
	w.Lock()
	defer w.Unlock()

	if w.closed.Get() == closedTrue {
		return terror.ErrWorkerAlreadyClosed.Generate()
	}

	st := w.subTaskHolder.findSubTask(cfg.Name)
	if st == nil {
		return terror.ErrWorkerSubTaskNotFound.Generate(cfg.Name)
	}

	w.l.Info("update sub task", zap.String("task", cfg.Name))
	return st.Update(cfg)
}

// OperateSubTask stop/resume/pause  sub task
func (w *Worker) OperateSubTask(name string, op pb.TaskOp) error {
	w.Lock()
	defer w.Unlock()

	if w.closed.Get() == closedTrue {
		return terror.ErrWorkerAlreadyClosed.Generate()
	}

	st := w.subTaskHolder.findSubTask(name)
	if st == nil {
		return terror.ErrWorkerSubTaskNotFound.Generate(name)
	}

	var err error
	switch op {
	case pb.TaskOp_Stop:
		w.l.Info("stop sub task", zap.String("task", name))
		st.Close()
		w.subTaskHolder.removeSubTask(name)
	case pb.TaskOp_Pause:
		w.l.Info("pause sub task", zap.String("task", name))
		err = st.Pause()
	case pb.TaskOp_Resume:
		w.l.Info("resume sub task", zap.String("task", name))
		err = st.Resume()
	case pb.TaskOp_AutoResume:
		w.l.Info("auto_resume sub task", zap.String("task", name))
		err = st.Resume()
	default:
		err = terror.ErrWorkerUpdateTaskStage.Generatef("invalid operate %s on subtask %v", op, name)
	}

	return err
}

// QueryStatus query worker's sub tasks' status
func (w *Worker) QueryStatus(ctx context.Context, name string) []*pb.SubTaskStatus {
	w.RLock()
	defer w.RUnlock()

	if w.closed.Get() == closedTrue {
		w.l.Warn("querying status from a closed worker")
		return nil
	}

	// use one timeout for all tasks. increase this value if it's too short.
	ctx2, cancel2 := context.WithTimeout(ctx, utils.DefaultDBTimeout)
	defer cancel2()
	return w.Status(ctx2, name)
}

func (w *Worker) resetSubtaskStage() (int64, error) {
	subTaskStages, subTaskCfgm, revSubTask, err := w.fetchSubTasksAndAdjust()
	if err != nil {
		return 0, err
	}
	// use sts to check which subtask has no subtaskCfg or subtaskStage now
	sts := w.subTaskHolder.getAllSubTasks()
	for name, subtaskCfg := range subTaskCfgm {
		stage, ok := subTaskStages[name]
		if ok {
			// TODO: right operation sequences may get error when we get etcdErrCompact, need to handle it later
			// For example, Expect: Running -(pause)-> Paused -(resume)-> Running
			// we get an etcd compact error at the first running. If we try to "resume" it now, we will get an error
			opType, err2 := w.operateSubTaskStage(stage, subtaskCfg)
			if err2 != nil {
				opErrCounter.WithLabelValues(w.name, opType).Inc()
				log.L().Error("fail to operate subtask stage", zap.Stringer("stage", stage),
					zap.String("task", subtaskCfg.Name), zap.Error(err2))

			}
			delete(sts, name)
		}
	}
	// remove subtasks without subtask config or subtask stage
	for name := range sts {
		err = w.OperateSubTask(name, pb.TaskOp_Stop)
		if err != nil {
			opErrCounter.WithLabelValues(w.name, pb.TaskOp_Stop.String()).Inc()
			log.L().Error("fail to stop subtask", zap.String("task", name), zap.Error(err))
		}
	}
	return revSubTask, nil
}

func (w *Worker) observeSubtaskStage(ctx context.Context, etcdCli *clientv3.Client, rev int64) error {
	var wg sync.WaitGroup

	for {
		subTaskStageCh := make(chan ha.Stage, 10)
		subTaskErrCh := make(chan error, 10)
		wg.Add(1)
		// use ctx1, cancel1 to make sure old watcher has been released
		ctx1, cancel1 := context.WithCancel(ctx)
		go func() {
			defer func() {
				close(subTaskStageCh)
				close(subTaskErrCh)
				wg.Done()
			}()
			ha.WatchSubTaskStage(ctx1, etcdCli, w.cfg.SourceID, rev+1, subTaskStageCh, subTaskErrCh)
		}()
		err := w.handleSubTaskStage(ctx1, subTaskStageCh, subTaskErrCh)
		cancel1()
		wg.Wait()

		if etcdutil.IsRetryableError(err) {
			rev = 0
			retryNum := 1
			for rev == 0 {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(500 * time.Millisecond):
					rev, err = w.resetSubtaskStage()
					if err != nil {
						log.L().Error("resetSubtaskStage is failed, will retry later", zap.Error(err), zap.Int("retryNum", retryNum))
					}
				}
				retryNum++
			}
		} else {
			if err != nil {
				log.L().Error("observeSubtaskStage is failed and will quit now", zap.Error(err))
			} else {
				log.L().Info("observeSubtaskStage will quit now")
			}
			return err
		}
	}
}

func (w *Worker) handleSubTaskStage(ctx context.Context, stageCh chan ha.Stage, errCh chan error) error {
	closed := false
	for {
		select {
		case <-ctx.Done():
			closed = true
		case stage, ok := <-stageCh:
			if !ok {
				closed = true
				break
			}
			log.L().Info("receive subtask stage change", zap.Stringer("stage", stage), zap.Bool("is deleted", stage.IsDeleted))
			opType, err := w.operateSubTaskStageWithoutConfig(stage)
			if err != nil {
				opErrCounter.WithLabelValues(w.name, opType).Inc()
				log.L().Error("fail to operate subtask stage", zap.Stringer("stage", stage), zap.Bool("is deleted", stage.IsDeleted), zap.Error(err))
				if etcdutil.IsRetryableError(err) {
					return err
				}
			}
		case err, ok := <-errCh:
			if !ok {
				closed = true
				break
			}
			// TODO: deal with err
			log.L().Error("WatchSubTaskStage received an error", zap.Error(err))
			if etcdutil.IsRetryableError(err) {
				return err
			}
		}
		if closed {
			log.L().Info("worker is closed, handleSubTaskStage will quit now")
			return nil
		}
	}
}

// operateSubTaskStage returns TaskOp.String() additionally to record metrics
func (w *Worker) operateSubTaskStage(stage ha.Stage, subTaskCfg config.SubTaskConfig) (string, error) {
	var op pb.TaskOp
	switch {
	case stage.Expect == pb.Stage_Running, stage.Expect == pb.Stage_Paused:
		if st := w.subTaskHolder.findSubTask(stage.Task); st == nil {
			// create the subtask for expected running and paused stage.
			log.L().Info("start to create subtask", zap.String("sourceID", subTaskCfg.SourceID), zap.String("task", subTaskCfg.Name))
			err := w.StartSubTask(&subTaskCfg, stage.Expect)
			return opErrTypeBeforeOp, err
		}
		if stage.Expect == pb.Stage_Running {
			op = pb.TaskOp_Resume
		} else if stage.Expect == pb.Stage_Paused {
			op = pb.TaskOp_Pause
		}
	case stage.IsDeleted:
		op = pb.TaskOp_Stop
	}
	return op.String(), w.OperateSubTask(stage.Task, op)
}

// operateSubTaskStageWithoutConfig returns TaskOp additionally to record metrics
func (w *Worker) operateSubTaskStageWithoutConfig(stage ha.Stage) (string, error) {
	var subTaskCfg config.SubTaskConfig
	if stage.Expect == pb.Stage_Running {
		if st := w.subTaskHolder.findSubTask(stage.Task); st == nil {
			tsm, _, err := ha.GetSubTaskCfg(w.etcdClient, stage.Source, stage.Task, stage.Revision)
			if err != nil {
				// TODO: need retry
				return opErrTypeBeforeOp, terror.Annotate(err, "fail to get subtask config from etcd")
			}
			var ok bool
			if subTaskCfg, ok = tsm[stage.Task]; !ok {
				return opErrTypeBeforeOp, terror.ErrWorkerFailToGetSubtaskConfigFromEtcd.Generate(stage.Task)
			}
		}
	}
	return w.operateSubTaskStage(stage, subTaskCfg)
}

func (w *Worker) observeRelayStage(ctx context.Context, etcdCli *clientv3.Client, rev int64) error {
	var wg sync.WaitGroup
	for {
		relayStageCh := make(chan ha.Stage, 10)
		relayErrCh := make(chan error, 10)
		wg.Add(1)
		// use ctx1, cancel1 to make sure old watcher has been released
		ctx1, cancel1 := context.WithCancel(ctx)
		go func() {
			defer func() {
				close(relayStageCh)
				close(relayErrCh)
				wg.Done()
			}()
			ha.WatchRelayStage(ctx1, etcdCli, w.cfg.SourceID, rev+1, relayStageCh, relayErrCh)
		}()
		err := w.handleRelayStage(ctx1, relayStageCh, relayErrCh)
		cancel1()
		wg.Wait()

		if etcdutil.IsRetryableError(err) {
			rev = 0
			retryNum := 1
			for rev == 0 {
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(500 * time.Millisecond):
					stage, rev1, err1 := ha.GetRelayStage(etcdCli, w.cfg.SourceID)
					if err1 != nil {
						log.L().Error("get source bound from etcd failed, will retry later", zap.Error(err1), zap.Int("retryNum", retryNum))
						break
					}
					rev = rev1
					if stage.IsEmpty() {
						stage.IsDeleted = true
					}
					opType, err1 := w.operateRelayStage(ctx, stage)
					if err1 != nil {
						opErrCounter.WithLabelValues(w.name, opType).Inc()
						log.L().Error("fail to operate relay", zap.Stringer("stage", stage), zap.Bool("is deleted", stage.IsDeleted), zap.Error(err1))
					}
				}
				retryNum++
			}
		} else {
			if err != nil {
				log.L().Error("observeRelayStage is failed and will quit now", zap.Error(err))
			} else {
				log.L().Info("observeRelayStage will quit now")
			}
			return err
		}
	}
}

func (w *Worker) handleRelayStage(ctx context.Context, stageCh chan ha.Stage, errCh chan error) error {
OUTER:
	for {
		select {
		case <-ctx.Done():
			break OUTER
		case stage, ok := <-stageCh:
			if !ok {
				break OUTER
			}
			log.L().Info("receive relay stage change", zap.Stringer("stage", stage), zap.Bool("is deleted", stage.IsDeleted))
			opType, err := w.operateRelayStage(ctx, stage)
			if err != nil {
				opErrCounter.WithLabelValues(w.name, opType).Inc()
				log.L().Error("fail to operate relay", zap.Stringer("stage", stage), zap.Bool("is deleted", stage.IsDeleted), zap.Error(err))
			}
		case err, ok := <-errCh:
			if !ok {
				break OUTER
			}
			log.L().Error("WatchRelayStage received an error", zap.Error(err))
			if etcdutil.IsRetryableError(err) {
				return err
			}
		}
	}
	log.L().Info("worker is closed, handleRelayStage will quit now")
	return nil
}

// operateRelayStage returns RelayOp.String() additionally to record metrics
// *RelayOp is nil only when error is nil, so record on error will not meet nil-pointer deference
func (w *Worker) operateRelayStage(ctx context.Context, stage ha.Stage) (string, error) {
	var op pb.RelayOp
	switch {
	case stage.Expect == pb.Stage_Running:
		if w.relayHolder.Stage() == pb.Stage_New {
			w.relayHolder.Start()
			w.relayPurger.Start()
			return opErrTypeBeforeOp, nil
		}
		op = pb.RelayOp_ResumeRelay
	case stage.Expect == pb.Stage_Paused:
		op = pb.RelayOp_PauseRelay
	case stage.IsDeleted:
		op = pb.RelayOp_StopRelay
	}
	return op.String(), w.operateRelay(ctx, op)
}

// OperateRelay operates relay unit
func (w *Worker) operateRelay(ctx context.Context, op pb.RelayOp) error {
	if w.closed.Get() == closedTrue {
		return terror.ErrWorkerAlreadyClosed.Generate()
	}

	if w.relayHolder != nil {
		return w.relayHolder.Operate(ctx, op)
	}

	w.l.Warn("enable-relay is false, ignore operate relay")
	return nil
}

// PurgeRelay purges relay log files
func (w *Worker) PurgeRelay(ctx context.Context, req *pb.PurgeRelayRequest) error {
	if w.closed.Get() == closedTrue {
		return terror.ErrWorkerAlreadyClosed.Generate()
	}

	if w.relayPurger != nil {
		return w.relayPurger.Do(ctx, req)
	}

	w.l.Warn("enable-relay is false, ignore purge relay")
	return nil
}

// ForbidPurge implements PurgeInterceptor.ForbidPurge
func (w *Worker) ForbidPurge() (bool, string) {
	if w.closed.Get() == closedTrue {
		return false, ""
	}

	// forbid purging if some sub tasks are paused, so we can debug the system easily
	// This function is not protected by `w.RWMutex`, which may lead to sub tasks information
	// not up to date, but do not affect correctness.
	for _, st := range w.subTaskHolder.getAllSubTasks() {
		stage := st.Stage()
		if stage == pb.Stage_New || stage == pb.Stage_Paused {
			return true, fmt.Sprintf("sub task %s current stage is %s", st.cfg.Name, stage.String())
		}
	}
	return false, ""
}

// OperateSchema operates schema for an upstream table.
func (w *Worker) OperateSchema(ctx context.Context, req *pb.OperateWorkerSchemaRequest) (schema string, err error) {
	w.Lock()
	defer w.Unlock()

	if w.closed.Get() == closedTrue {
		return "", terror.ErrWorkerAlreadyClosed.Generate()
	}

	st := w.subTaskHolder.findSubTask(req.Task)
	if st == nil {
		return "", terror.ErrWorkerSubTaskNotFound.Generate(req.Task)
	}

	return st.OperateSchema(ctx, req)
}

// copyConfigFromSource copies config items from source config to sub task
func copyConfigFromSource(cfg *config.SubTaskConfig, sourceCfg *config.SourceConfig) error {
	cfg.From = sourceCfg.From

	cfg.Flavor = sourceCfg.Flavor
	cfg.ServerID = sourceCfg.ServerID
	cfg.RelayDir = sourceCfg.RelayDir
	cfg.EnableGTID = sourceCfg.EnableGTID
	cfg.UseRelay = sourceCfg.EnableRelay

	// we can remove this from SubTaskConfig later, because syncer will always read from relay
	cfg.AutoFixGTID = sourceCfg.AutoFixGTID

	if cfg.CaseSensitive != sourceCfg.CaseSensitive {
		log.L().Warn("different case-sensitive config between task config and source config, use `true` for it.")
	}
	cfg.CaseSensitive = cfg.CaseSensitive || sourceCfg.CaseSensitive
	filter, err := bf.NewBinlogEvent(cfg.CaseSensitive, cfg.FilterRules)
	if err != nil {
		return err
	}

	for _, filterRule := range sourceCfg.Filters {
		if err = filter.AddRule(filterRule); err != nil {
			// task level config has higher priority
			if errors.IsAlreadyExists(errors.Cause(err)) {
				log.L().Warn("filter config already exist in source config, overwrite it", log.ShortError(err))
				continue
			}
			return err
		}
		cfg.FilterRules = append(cfg.FilterRules, filterRule)
	}
	return nil
}

// copyConfigFromSourceForEach do copyConfigFromSource for each value in subTaskCfgM and change subTaskCfgM in-place
func copyConfigFromSourceForEach(subTaskCfgM map[string]config.SubTaskConfig, sourceCfg *config.SourceConfig) error {
	for k, subTaskCfg := range subTaskCfgM {
		if err2 := copyConfigFromSource(&subTaskCfg, sourceCfg); err2 != nil {
			return err2
		}
		subTaskCfgM[k] = subTaskCfg
	}
	return nil
}

// getAllSubTaskStatus returns all subtask status of this worker, note the field
// in subtask status is not completed, only includes `Name`, `Stage` and `Result` now
func (w *Worker) getAllSubTaskStatus() map[string]*pb.SubTaskStatus {
	sts := w.subTaskHolder.getAllSubTasks()
	result := make(map[string]*pb.SubTaskStatus, len(sts))
	for name, st := range sts {
		st.RLock()
		result[name] = &pb.SubTaskStatus{
			Name:   name,
			Stage:  st.stage,
			Result: proto.Clone(st.result).(*pb.ProcessResult),
		}
		st.RUnlock()
	}
	return result
}

// HandleError handle worker error
func (w *Worker) HandleError(ctx context.Context, req *pb.HandleWorkerErrorRequest) error {
	w.Lock()
	defer w.Unlock()

	if w.closed.Get() == closedTrue {
		return terror.ErrWorkerAlreadyClosed.Generate()
	}

	st := w.subTaskHolder.findSubTask(req.Task)
	if st == nil {
		return terror.ErrWorkerSubTaskNotFound.Generate(req.Task)
	}

	return st.HandleError(ctx, req)
}

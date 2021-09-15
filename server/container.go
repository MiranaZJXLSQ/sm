// Copyright 2021 The entertainment-venue Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/coreos/etcd/clientv3/concurrency"
	"github.com/entertainment-venue/sm/pkg/apputil"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type serverContainer struct {
	*apputil.Container

	// 存下来，方便一些管理逻辑
	id, service string
	// 管理孩子goroutine
	cancel context.CancelFunc

	// 管理自己的goroutine
	stopper *apputil.GoroutineStopper

	ew *etcdWrapper

	mu     sync.Mutex
	shards map[string]*serverShard
	// serverShard sm管理很多业务app，不同业务app有不同的task节点，这块做个map，可能出现单container负责多个app的场景
	srvOps  map[string]*operator
	stopped bool // container进入stopped状态

	eq *eventQueue

	lg *zap.Logger

	// 保证sm运行健康的goroutine，通过task节点下发任务给op
	mtWorker *maintenanceWorker

	// op需要监听特定app的task在etcd中的节点，保证app级别只有一个，sm放在leader中
	op *operator
}

func newServerContainer(ctx context.Context, lg *zap.Logger, id, service string) (*serverContainer, error) {
	ctx, cancel := context.WithCancel(ctx)

	// Container只关注通用部分，所以service和id还是要保留一份到数据结构
	sc := serverContainer{
		lg:      lg,
		service: service,
		id:      id,
		cancel:  cancel,
		ew:      newEtcdWrapper(),
		eq:      newEventQueue(ctx, lg),
	}

	sc.mtWorker = newMaintenanceWorker(&sc, sc.service)

	sc.stopper.Wrap(
		func(ctx context.Context) {
			sc.campaignLeader(ctx)
		})

	return &sc, nil
}

func (c *serverContainer) Close() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()

	// stop serverShard
	for _, s := range c.shards {
		s.Close()
	}

	// stop operator
	for _, o := range c.srvOps {
		o.Close()
	}

	c.lg.Info("close serverContainer",
		zap.String("id", c.id),
		zap.String("service", c.service),
	)
}

func (c *serverContainer) Add(ctx context.Context, id string, spec *apputil.ShardSpec) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		c.lg.Info("container stopped",
			zap.String("id", id),
			zap.String("service", c.service),
		)
		return nil
	}

	if _, ok := c.shards[id]; ok {
		c.lg.Info("shard existed",
			zap.String("id", id),
			zap.String("service", c.service),
		)
		return nil
	}

	shard, err := startShard(ctx, c.lg, c, id, spec)
	if err != nil {
		return errors.Wrap(err, "")
	}
	c.shards[id] = shard
	return nil
}

func (c *serverContainer) Drop(_ context.Context, id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		c.lg.Info("container stopped",
			zap.String("id", id),
			zap.String("service", c.service),
		)
		return nil
	}

	sd, ok := c.shards[id]
	if !ok {
		return errNotExist
	}
	sd.Close()
	return nil
}

func (c *serverContainer) Load(ctx context.Context, id string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		c.lg.Info("container stopped",
			zap.String("id", id),
			zap.String("service", c.service),
		)
		return "", nil
	}

	sd, ok := c.shards[id]
	if !ok {
		return "", errNotExist
	}
	return sd.getLoad(), nil
}

func (c *serverContainer) NewOp(service string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.stopped {
		c.lg.Info("container stopped",
			zap.String("service", service),
		)
		return nil
	}

	if _, ok := c.srvOps[service]; !ok {
		op, err := newOperator(c, service)
		if err != nil {
			return errors.Wrap(err, "")
		}
		c.srvOps[service] = op
	}
	return nil
}

type leaderEtcdValue struct {
	ContainerId string `json:"containerId"`
	CreateTime  string `json:"createTime"`
}

func (v *leaderEtcdValue) String() string {
	b, _ := json.Marshal(v)
	return string(b)
}

func (c *serverContainer) campaignLeader(ctx context.Context) {
	for {
	loop:
		select {
		case <-ctx.Done():
			c.lg.Info("leader exit campaignLeader", zap.String("service", c.service))
			return
		default:
		}

		leaderNodePrefix := c.ew.leaderNode(c.service)
		lvalue := leaderEtcdValue{ContainerId: c.id, CreateTime: time.Now().String()}
		election := concurrency.NewElection(c.Session, leaderNodePrefix)
		if err := election.Campaign(ctx, lvalue.String()); err != nil {
			c.lg.Error("failed to campaignLeader", zap.Error(err))
			time.Sleep(defaultSleepTimeout)
			goto loop
		}
		c.lg.Info("campaignLeader success",
			zap.String("service", c.service),
			zap.String("leaderNodePrefix", leaderNodePrefix),
			zap.Int64("lease", int64(c.Session.Lease())),
		)

		// leader启动时，等待一个时间段，方便所有container做至少一次heartbeat，然后开始监测是否需要进行container和shard映射关系的变更。
		// etcd sdk中keepalive的请求发送时间时500ms，3s>>500ms，认为这个时间段内，所有container都会发heartbeat，不存在的就认为没有任务。
		time.Sleep(5 * time.Second)

		if err := c.leaderStartDistribution(ctx); err != nil {
			c.lg.Error("leader failed to leaderStartDistribution", zap.Error(err))
			time.Sleep(defaultSleepTimeout)
			goto loop
		}

		// leader需要处理shard move的任务
		var err error
		c.op, err = newOperator(c, c.service)
		if err != nil {
			c.lg.Error("leader failed to newOperator", zap.Error(err))
			time.Sleep(defaultSleepTimeout)
			goto loop
		}

		// 检查所有shard应该都被分配container，当前app的配置信息是预先录入etcd的。此时提取该信息，得到所有shard的id，
		// https://github.com/entertainment-venue/sm/wiki/leader%E8%AE%BE%E8%AE%A1%E6%80%9D%E8%B7%AF
		go c.mtWorker.Start()

		// block until出现需要放弃leader职权的事件
		c.lg.Info("leader completed op", zap.String("service", c.service))
		select {
		case <-ctx.Done():
			c.lg.Info("leader exit", zap.String("service", c.service))
			return
		}
	}
}

func (c *serverContainer) leaderStartDistribution(ctx context.Context) error {
	// 先把当前的分配关系下发下去，和static membership，不过我们场景是由单点完成的，由性能瓶颈，但是不像LRMF场景下serverless难以判断正确性
	// 分配关系下发，解决的是先把现有分配关系搞下去，然后再通过shardAllocateLoop检验是否需要整体进行shard move，相当于init
	// TODO app接入数量一个公司可控，所以方案可行
	bdShardNode := c.ew.nodeAppShard(c.service)
	curShardIdAndValue, err := c.Client.GetKVs(ctx, bdShardNode)
	if err != nil {
		return errors.Wrap(err, "")
	}
	var moveActions moveActionList
	for shardId, value := range curShardIdAndValue {
		var ss apputil.ShardSpec
		if err := json.Unmarshal([]byte(value), &ss); err != nil {
			return errors.Wrap(err, "")
		}

		// 未分配container的shard，不需要move指令下发
		if ss.ContainerId != "" {
			// 下发指令，接受不了的直接干掉当前的分配关系
			ma := moveAction{Service: c.service, ShardId: shardId, AddEndpoint: ss.ContainerId, AllowDrop: true}
			moveActions = append(moveActions, &ma)

			c.lg.Info("leaderStartDistribution shard move action",
				zap.String("service", c.service),
				zap.Object("action", &ma),
			)
		}
	}
	// 向自己的app任务节点发任务
	if len(moveActions) == 0 {
		c.lg.Info("leaderStartDistribution no move action created", zap.String("service", c.service))
		return nil
	}

	item := Item{
		Value:    moveActions.String(),
		Priority: time.Now().Unix(),
	}
	c.eq.push(&item, true)
	return nil
}

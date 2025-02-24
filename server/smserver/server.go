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

package smserver

import (
	"github.com/entertainment-venue/sm/pkg/apputil"
	_ "github.com/entertainment-venue/sm/server/docs"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	swaggerfiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
	"go.uber.org/zap"
)

type Server struct {
	shardServer *apputil.ShardServer
	smContainer *smContainer

	opts  *serverOptions
	donec chan struct{}
}

type serverOptions struct {
	// id是当前容器/进程的唯一标记，不能变化，用于做container和shard的映射关系
	id string

	// 业务app所在的服务注册发现系统的唯一标记，是业务的别名
	service string

	// etcd集群的配置
	endpoints []string

	// 监听端口: 提供管理职能，add、drop
	addr string

	lg *zap.Logger

	// etcdPrefix 这个路径是etcd中开辟出来给sm使用的，etcd可能是多个组件公用
	// TODO 要有用户名和密码限制
	etcdPrefix string
}

type ServerOption func(options *serverOptions)

func WithId(v string) ServerOption {
	return func(options *serverOptions) {
		options.id = v
	}
}

func WithService(v string) ServerOption {
	return func(options *serverOptions) {
		options.service = v
	}
}

func WithEndpoints(v []string) ServerOption {
	return func(options *serverOptions) {
		options.endpoints = v
	}
}

func WithAddr(v string) ServerOption {
	return func(options *serverOptions) {
		options.addr = v
	}
}

func WithLogger(v *zap.Logger) ServerOption {
	return func(options *serverOptions) {
		options.lg = v
	}
}

func WithEtcdPrefix(v string) ServerOption {
	return func(options *serverOptions) {
		options.etcdPrefix = v
	}
}

func NewServer(fn ...ServerOption) (*Server, error) {
	ops := serverOptions{}
	for _, f := range fn {
		f(&ops)
	}

	if ops.id == "" {
		return nil, errors.New("id err")
	}
	if ops.service == "" {
		return nil, errors.New("service err")
	}
	if ops.addr == "" {
		return nil, errors.New("addr err")
	}
	if len(ops.endpoints) == 0 {
		return nil, errors.New("endpoints err")
	}
	if ops.lg == nil {
		return nil, errors.New("logger err")
	}
	apputil.InitEtcdPrefix(ops.etcdPrefix)

	srv := Server{opts: &ops, donec: make(chan struct{})}
	if err := srv.run(); err != nil {
		return nil, err
	}

	go func() {
		select {
		// 主动关闭: Close方法调用
		case <-srv.donec:
			ops.lg.Info(
				"server active exit",
				zap.String("service", srv.opts.service),
			)
			// 主动关闭可以直接退出goroutine
			return

		// 被动关闭: 观测ShardServer或者smContainer都预Session相关退出，可能因为session的关闭导致
		case <-srv.shardServer.Done():
			srv.close()
			ops.lg.Info("server passive exit")

			// 尝试重启
			for {
				select {
				case <-srv.donec:
					ops.lg.Info(
						"server active exit when retry run server",
						zap.String("service", ops.service),
					)
					return
				default:
				}
				// 监控异常关闭，不退出服务，container需要刷新
				err := srv.run()
				if err == nil {
					break
				}
				ops.lg.Error(
					"run error",
					zap.String("service", ops.service),
					zap.Error(err),
				)
			}
		}
	}()

	return &srv, nil
}

func (s *Server) run() error {
	container, err := apputil.NewContainer(
		apputil.ContainerWithService(s.opts.service),
		apputil.ContainerWithId(s.opts.id),
		apputil.ContainerWithEndpoints(s.opts.endpoints),
		apputil.ContainerWithLogger(s.opts.lg))
	if err != nil {
		return errors.Wrap(err, "")
	}

	smContainer, err := newSMContainer(s.opts.lg, container)
	if err != nil {
		container.Close()
		return errors.Wrap(err, "")
	}
	s.smContainer = smContainer

	ss, err := apputil.NewShardServer(
		apputil.ShardServerWithAddr(s.opts.addr),
		apputil.ShardServerWithContainer(container),
		apputil.ShardServerWithApiHandler(s.getHandlers(smContainer)),
		apputil.ShardServerWithShardImplementation(smContainer),
		apputil.ShardServerWithLogger(s.opts.lg),
		apputil.ShardServerWithEtcdPrefix(s.opts.etcdPrefix))
	if err != nil {
		container.Close()
		smContainer.Close()
		return errors.Wrap(err, "new shard server failed")
	}
	s.shardServer = ss
	return nil
}

// Close 在进程收到退出信号时触发，和NewServer中的goroutine可能并发执行，
// shardServer的Close是threadsafe的，但是shardServer的Done先触发被动关闭，close方法会被调用两次，
// 虽然smContainer的Close是threadsafe，但两个组件会被关闭两次，请发发生比较少
func (s *Server) Close() {
	// 主动关闭: 需要关闭shardServer
	// shardServer的关闭会触发NewServer中的goroutine被动关闭
	s.shardServer.Close()

	// 通知调用方，因为是主动关闭
	close(s.donec)

	// 关闭后，进程退出，至于smContainer的关闭依赖shardServer即可
}

func (s *Server) close() {
	defer s.opts.lg.Sync()
	s.smContainer.Close()
}

func (s *Server) Done() <-chan struct{} {
	return s.donec
}

func (s *Server) getHandlers(container *smContainer) map[string]func(c *gin.Context) {
	apiSrv := newSMShardApi(container)
	handlers := make(map[string]func(c *gin.Context))
	handlers["/sm/server/add-spec"] = apiSrv.GinAddSpec
	handlers["/sm/server/del-spec"] = apiSrv.GinDelSpec
	handlers["/sm/server/get-spec"] = apiSrv.GinGetSpec
	handlers["/sm/server/update-spec"] = apiSrv.GinUpdateSpec
	handlers["/sm/server/add-shard"] = apiSrv.GinAddShard
	handlers["/sm/server/del-shard"] = apiSrv.GinDelShard
	handlers["/sm/server/get-shard"] = apiSrv.GinGetShard
	handlers["/swagger/*any"] = ginSwagger.WrapHandler(swaggerfiles.Handler)
	return handlers
}

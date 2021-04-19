// Copyright (C) 2019-2020 Zilliz. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software distributed under the License
// is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express
// or implied. See the License for the specific language governing permissions and limitations under the License.

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/zilliztech/milvus-distributed/internal/logutil"

	"go.uber.org/zap"

	dn "github.com/zilliztech/milvus-distributed/internal/datanode"

	distributed "github.com/zilliztech/milvus-distributed/cmd/distributed/components"
	"github.com/zilliztech/milvus-distributed/internal/log"
	"github.com/zilliztech/milvus-distributed/internal/msgstream"
)

func main() {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	msFactory := msgstream.NewPmsFactory()
	dn.Params.Init()
	logutil.SetupLogger(&dn.Params.Log)

	dn, err := distributed.NewDataNode(ctx, msFactory)
	if err != nil {
		panic(err)
	}
	if err = dn.Run(); err != nil {
		panic(err)
	}

	sc := make(chan os.Signal, 1)
	signal.Notify(sc,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)

	sig := <-sc
	log.Debug("Got signal to exit signal", zap.String("signal", sig.String()))

	err = dn.Stop()
	if err != nil {
		panic(err)
	}
}

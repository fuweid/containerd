//go:build linux
// +build linux

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"sync"
	"testing"
	"time"

	criconfig "github.com/containerd/containerd/pkg/cri/config"
	criserver "github.com/containerd/containerd/pkg/cri/server"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"

	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func BenchmarkCRIList13Images(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		_, err := criImageService13.ListImages(ctx, nil)
		assert.NoError(b, err)
	}
}

func BenchmarkCRIList26Images(b *testing.B) {
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		_, err := criImageService26.ListImages(ctx, nil)
		assert.NoError(b, err)
	}
}

func initForBenchmarkCRIImageList() {
	logrus.SetLevel(logrus.WarnLevel)

	if err := os.MkdirAll(rootDir13, 0666); err != nil {
		panic(fmt.Errorf("failed to create %s: %v", rootDir13, err))
	}
	if err := os.MkdirAll(rootDir26, 0666); err != nil {
		panic(fmt.Errorf("failed to create %s: %v", rootDir26, err))
	}

	criImageService13 = initLocalCRIImageService(rootDir13)
	// un-comment it if first time to run
	// prepareCRIImages(criImageService13, imageDescs[:1])

	criImageService26 = initLocalCRIImageService(rootDir26)
	// un-comment it if first time to run
	// prepareCRIImages(criImageService26, imageDescs)

	goruntime.GC()
}

var (
	criImageService13 runtime.ImageServiceServer
	rootDir13         = "/tmp/containerd-cri-benchmark-13"

	criImageService26 runtime.ImageServiceServer
	rootDir26         = "/tmp/containerd-cri-benchmark-26"
)

type imageDesc struct {
	repoName string
	tags     []string
}

var imageDescs = []imageDesc{
	{
		repoName: "docker.io/library/busybox",
		tags: []string{
			"1.23", "1.24", "1.25", "1.26", "1.27", "1.28", "1.29",
			"1.30", "1.31", "1.32", "1.33", "1.34", "1.35",
		},
	},
	{
		repoName: "docker.io/library/alpine",
		tags: []string{
			"2.6", "2.7", "3.1", "3.2", "3.3", "3.4", "3.5",
			"3.6", "3.7", "3.8", "3.9", "3.10", "3.11",
		},
	},
}

func prepareCRIImages(srv runtime.ImageServiceServer, descs []imageDesc) {
	ctx := context.Background()
	for _, images := range descs {
		var wg sync.WaitGroup

		for _, tag := range images.tags {
			wg.Add(1)
			target := images.repoName + ":" + tag

			go func() {
				defer wg.Done()

				_, err := srv.PullImage(ctx, &runtime.PullImageRequest{
					Image: &runtime.ImageSpec{
						Image: target,
					},
				})
				if err != nil {
					panic(err)
				}
			}()
		}

		wg.Wait()
	}
}

func initLocalCRIImageService(tmpDir string) runtime.ImageServiceServer {
	cli := buildLocalContainerdClient(nil, tmpDir)

	containerdRootDir := filepath.Join(tmpDir, "root")
	criWorkDir := filepath.Join(tmpDir, "cri-plugin")

	cfg := criconfig.Config{
		PluginConfig:      criconfig.DefaultConfig(),
		ContainerdRootDir: containerdRootDir,
		RootDir:           filepath.Join(criWorkDir, "root"),
		StateDir:          filepath.Join(criWorkDir, "state"),
	}

	criPlugin, err := criserver.NewCRIService(cfg, cli)
	if err != nil {
		panic(fmt.Errorf("failed to init cri plugin: %v", err))
	}

	errCh := make(chan error)
	go func() {
		defer close(errCh)

		errCh <- criPlugin.Run()
	}()

	service := criPlugin.(interface {
		InstrumentedImageService() runtime.ImageServiceServer
	}).InstrumentedImageService()

	ctx := context.Background()
	for {
		select {
		case err := <-errCh:
			panic(fmt.Errorf("unexpected error: %v", err))
		default:
		}

		time.Sleep(100 * time.Millisecond)
		_, err := service.ImageFsInfo(ctx, nil)
		if err == nil {
			break
		}

		if err.Error() == "server is not initialized yet" {
			continue
		}
		panic(fmt.Errorf("unexpected error from ImageFsInfo: %v", err))
	}
	return service
}

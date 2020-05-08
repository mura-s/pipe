// Copyright 2020 The PipeCD Authors.
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

package git

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Client is a git client for cloning/fetching git repo.
// It keeps a local cache for faster future cloning.
type Client interface {
	Clone(ctx context.Context, base, repoFullName, destination string) (Repo, error)
	GetLatestRemoteHashForBranch(ctx context.Context, remote, branch string) (string, error)
	Clean() error
}

type client struct {
	username  string
	email     string
	gitPath   string
	cacheDir  string
	mu        sync.Mutex
	repoLocks map[string]*sync.Mutex
	logger    *zap.Logger
}

// NewClient creates a new CLient instance for cloning git repositories.
// After using Clean should be called to delete cache data.
func NewClient(username, email string, logger *zap.Logger) (Client, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("unabled to find the path of git: %v", err)
	}

	cacheDir, err := ioutil.TempDir("", "gitcache")
	if err != nil {
		return nil, fmt.Errorf("unabled to create a temporary directory for git cache: %v", err)
	}

	return &client{
		username:  username,
		email:     email,
		gitPath:   gitPath,
		cacheDir:  cacheDir,
		repoLocks: make(map[string]*sync.Mutex),
		logger:    logger,
	}, nil
}

func (c *client) GetLatestRemoteHashForBranch(ctx context.Context, remote, branch string) (string, error) {
	ref := "refs/heads/" + branch
	out, err := retryCommand(3, time.Second, c.logger, func() ([]byte, error) {
		return c.runGitCommand(ctx, "", "ls-remote", ref)
	})
	if err != nil {
		c.logger.Error("failed to get latest remote hash for branch",
			zap.String("remote", remote),
			zap.String("branch", branch),
			zap.String("out", string(out)),
			zap.Error(err),
		)
		return "", err
	}
	parts := strings.Split(string(out), "\t")
	return parts[0], nil
}

// Clone clones a specific GitHub repository.
func (c *client) Clone(ctx context.Context, base, repoFullName, destination string) (Repo, error) {
	var (
		remote        = fmt.Sprintf("%s/%s", base, repoFullName)
		repoCachePath = filepath.Join(c.cacheDir, repoFullName) + ".git"
		logger        = c.logger.With(
			zap.String("base", base),
			zap.String("repo", repoFullName),
			zap.String("repo-cache-path", repoCachePath),
		)
	)

	c.lockRepo(repoFullName)
	defer c.unlockRepo(repoFullName)

	_, err := os.Stat(repoCachePath)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	if os.IsNotExist(err) {
		// Cache miss, clone for the first time.
		logger.Info(fmt.Sprintf("cloning %s for the first time", repoFullName))
		if err := os.MkdirAll(filepath.Dir(repoCachePath), os.ModePerm); err != nil && !os.IsExist(err) {
			return nil, err
		}
		out, err := retryCommand(3, time.Second, logger, func() ([]byte, error) {
			return c.runGitCommand(ctx, "", "clone", "--mirror", remote, repoCachePath)
		})
		if err != nil {
			logger.Error("failed to clone from remote",
				zap.String("out", string(out)),
				zap.Error(err),
			)
			return nil, fmt.Errorf("failed to clone from remote: %v", err)
		}
	} else {
		// Cache hit. Do a git fetch to keep updated.
		c.logger.Info(fmt.Sprintf("fetching %s to update the cache", repoFullName))
		out, err := retryCommand(3, time.Second, c.logger, func() ([]byte, error) {
			return c.runGitCommand(ctx, repoCachePath, "fetch")
		})
		if err != nil {
			logger.Error("failed to fetch from remote",
				zap.String("out", string(out)),
				zap.Error(err),
			)
			return nil, fmt.Errorf("failed to fetch: %v", err)
		}
	}

	err = os.MkdirAll(destination, os.ModePerm)
	if err != nil {
		return nil, err
	}

	if out, err := c.runGitCommand(ctx, "", "clone", repoCachePath, destination); err != nil {
		logger.Error("failed to clone from local",
			zap.String("out", string(out)),
			zap.String("repo-path", destination),
			zap.Error(err),
		)
		return nil, fmt.Errorf("failed to clone from local: %v", err)
	}

	r := NewRepo(repoFullName, destination, c.gitPath, remote, c.logger)
	if c.username != "" || c.email != "" {
		if err := r.SetUser(ctx, c.username, c.email); err != nil {
			return nil, fmt.Errorf("failed to set user: %v", err)
		}
	}

	return r, nil
}

// Clean removes all cache data.
func (c *client) Clean() error {
	return os.RemoveAll(c.cacheDir)
}

func (c *client) lockRepo(repoFullName string) {
	c.mu.Lock()
	if _, ok := c.repoLocks[repoFullName]; !ok {
		c.repoLocks[repoFullName] = &sync.Mutex{}
	}
	mu := c.repoLocks[repoFullName]
	c.mu.Unlock()

	mu.Lock()
}

func (c *client) unlockRepo(repoFullName string) {
	c.mu.Lock()
	c.repoLocks[repoFullName].Unlock()
	c.mu.Unlock()
}

func (c *client) runGitCommand(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.gitPath, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

// retryCommand retries a command a few times with a constant backoff.
func retryCommand(retries int, interval time.Duration, logger *zap.Logger, commander func() ([]byte, error)) (out []byte, err error) {
	for i := 0; i < retries; i++ {
		out, err = commander()
		if err == nil {
			return
		}
		logger.Warn(fmt.Sprintf("command was failed %d times, sleep %d seconds before retrying command", i+1, interval))
		time.Sleep(interval)
	}
	return
}
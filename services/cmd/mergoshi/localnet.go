package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"mergoshi/internal/near"
)

// The local network is a single near-sandbox node: the same runtime as
// testnet, on this machine, with no accounts or faucet needed.

const sandboxVersion = "2.13.4"

func sandboxPlatform() (string, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "Darwin-arm64", nil
	case "linux/amd64":
		return "Linux-x86_64", nil
	case "linux/arm64":
		return "Linux-aarch64", nil
	}
	return "", fmt.Errorf("near-sandbox has no build for %s/%s", runtime.GOOS, runtime.GOARCH)
}

// ensureSandbox downloads the near-sandbox binary once into dir/bin.
func ensureSandbox(ctx context.Context, dir string) (string, error) {
	bin := filepath.Join(dir, "bin", "near-sandbox")
	if p := os.Getenv("NEAR_SANDBOX_BIN_PATH"); p != "" {
		bin = p
	}
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	plat, err := sandboxPlatform()
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://s3-us-west-1.amazonaws.com/build.nearprotocol.com/nearcore/%s/%s/near-sandbox.tar.gz", plat, sandboxVersion)
	fmt.Println("downloading near-sandbox", sandboxVersion, "for", plat)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download near-sandbox: http %d", resp.StatusCode)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return "", errors.New("near-sandbox binary not found in the archive")
		}
		if err != nil {
			return "", err
		}
		if filepath.Base(h.Name) != "near-sandbox" || h.Typeflag != tar.TypeReg {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			return "", err
		}
		tmp := bin + ".tmp"
		f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(f, tr); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
		return bin, os.Rename(tmp, bin)
	}
}

// initHome creates the node's home and tunes it for a fast local game.
func initHome(bin, home, rpcPort, p2pPort string) error {
	if _, err := os.Stat(filepath.Join(home, "genesis.json")); err == nil {
		return nil
	}
	out, err := exec.Command(bin, "--home", home, "init").CombinedOutput()
	if err != nil {
		return fmt.Errorf("near-sandbox init: %v: %s", err, out)
	}
	path := filepath.Join(home, "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	cfg["rpc"].(map[string]any)["addr"] = "127.0.0.1:" + rpcPort
	cfg["network"].(map[string]any)["addr"] = "127.0.0.1:" + p2pPort
	cfg["consensus"].(map[string]any)["min_block_production_delay"] = map[string]any{"secs": 0, "nanos": 150_000_000}
	cfg["consensus"].(map[string]any)["max_block_production_delay"] = map[string]any{"secs": 0, "nanos": 600_000_000}
	raw, _ = json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(path, raw, 0o644)
}

func waitForRPC(ctx context.Context, c *near.Client) error {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := c.FinalBlock(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("the local node did not start; see .localnet/node.log")
}

// localNode starts a near-sandbox node in dir and returns a stop function.
func localNode(ctx context.Context, dir, port, p2pPort string) (*near.Client, func(), error) {
	home := filepath.Join(dir, "home")
	bin, err := ensureSandbox(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	if err := initHome(bin, home, port, p2pPort); err != nil {
		return nil, nil, err
	}
	logf, err := os.Create(filepath.Join(dir, "node.log"))
	if err != nil {
		return nil, nil, err
	}
	node := exec.Command(bin, "--home", home, "run")
	node.Stdout, node.Stderr = logf, logf
	if err := node.Start(); err != nil {
		logf.Close()
		return nil, nil, err
	}
	stop := func() {
		_ = node.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = node.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = node.Process.Kill()
		}
		logf.Close()
	}
	rpc := near.NewClient("http://127.0.0.1:" + port)
	if err := waitForRPC(ctx, rpc); err != nil {
		stop()
		return nil, nil, err
	}
	return rpc, stop, nil
}

// genesisAccount returns the sandbox's funded genesis account.
func genesisAccount(rpc *near.Client, dir string) (*near.Account, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "home", "validator_key.json"))
	if err != nil {
		return nil, err
	}
	var vk struct {
		AccountID string `json:"account_id"`
		SecretKey string `json:"secret_key"`
	}
	if err := json.Unmarshal(raw, &vk); err != nil {
		return nil, err
	}
	k, err := near.ParseSecretKey(vk.SecretKey)
	if err != nil {
		return nil, err
	}
	return near.NewAccount(rpc, vk.AccountID, k), nil
}

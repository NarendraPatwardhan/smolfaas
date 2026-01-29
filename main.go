// SmolFaaS - A Simple Function-as-a-Service Platform
//
// SmolFaaS is a lightweight FaaS platform designed for local development and deployment.
// It provides a command-line interface to manage serverless functions using Docker, BuildKit, and Caddy.
//
// Beyond being a functional tool, SmolFaaS is designed as a learning resource - similar to how
// SQLite serves as both a production database and an educational codebase. The entire implementation
// lives in a single, well-commented file that can be read top-to-bottom to understand how a FaaS system works.
//
// Architecture Overview:
//
//	┌─────────────────────────────────────────────────────────────────┐
//	│                         SmolFaaS CLI                            │
//	│                     (Cobra-based CLI)                           │
//	└─────────────────┬───────────────────────────────────────────────┘
//	                  │
//	    ┌─────────────┼─────────────┬─────────────────┐
//	    ▼             ▼             ▼                 ▼
//	┌────────┐  ┌───────────┐  ┌──────────┐  ┌─────────────┐
//	│BuildKit│  │  FaaS     │  │ Function │  │   Caddy     │
//	│Manager │  │  Builder  │  │ Manager  │  │   Manager   │
//	└────┬───┘  └─────┬─────┘  └────┬─────┘  └──────┬──────┘
//	     │            │             │               │
//	     ▼            ▼             ▼               ▼
//	┌────────┐  ┌───────────┐  ┌──────────┐  ┌─────────────┐
//	│BuildKit│  │ Runtime   │  │ Function │  │   Caddy     │
//	│Daemon  │  │ Handlers  │  │Containers│  │ Container   │
//	└────────┘  └───────────┘  └──────────┘  └─────────────┘
//
// Commands:
//   - smolfaas up: Build functions, start router, configure routes
//   - smolfaas down: Stop/remove functions and router
//   - smolfaas check: Verify consistency of state
//
// Design Principles:
//  1. Single-file architecture for educational readability
//  2. System-level commenting explaining design decisions
//  3. Content-based hashing (SQLite) for incremental builds
//  4. LLB-based Caddy image (no temp files)
//  5. Pluggable runtime support via RuntimeHandler interface

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"

	docker "github.com/docker/docker/client"
	_ "github.com/mattn/go-sqlite3"
	buildkit "github.com/moby/buildkit/client"
	_ "github.com/moby/buildkit/client/connhelper/dockercontainer"
	"github.com/moby/buildkit/client/llb"
	bkgw "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/moby/buildkit/util/progress/progressui"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
)

// ============================================================================
// Global Variables / Configuration
// ============================================================================
// These variables are populated by Cobra flags and shared across command handlers.

var (
	// CLI configuration flags
	functionsDir            string // Root directory containing function source code
	functionsAddr           string // Address/hostname for the Caddy router (e.g., "fn.localhost")
	networkName             string // Docker network shared by router and function containers
	caddyContainerName      string // Name for the Caddy router container
	buildkitContainerName   string // Name for the BuildKit daemon container
	buildkitCacheVolumeName string // Derived from buildkitContainerName
	faasImagePrefix         string // Prefix for function images and containers
	forceCleanup            bool   // Flag for destructive cleanup in 'down' command
	stateDBPath             string // Path to SQLite database for state persistence

	// Shared runtime state
	cmdCtx          context.Context
	cmdDockerClient *docker.Client
	logger          *slog.Logger
)

// ============================================================================
// Type Definitions
// ============================================================================

// RuntimeHandler defines the interface for language/runtime-specific FaaS logic.
// Each runtime (Go, Python, Node.js, Rust, Dockerfile) implements this interface
// to provide detection, LLB generation, and container command configuration.
type RuntimeHandler interface {
	// ID returns a unique identifier for the runtime (e.g., "go", "python", "node")
	ID() string

	// Version returns the default version of the runtime
	Version() string

	// Detect determines if a directory contains a function of this runtime type
	// by checking for runtime-specific marker files (go.mod, package.json, etc.)
	Detect(directoryPath string) bool

	// GenerateLLB creates the BuildKit LLB state for building a function image.
	// The LLB defines the complete build pipeline including base image, dependencies,
	// compilation, and final runtime image.
	GenerateLLB(funcDef FunctionDefinition) (llb.State, error)

	// Cmd returns the command to run inside the container (e.g., ["/app/server"])
	Cmd() []string
}

// FunctionDefinition holds all metadata for a discovered FaaS function.
// It tracks the function's source, runtime, naming conventions, and build state.
type FunctionDefinition struct {
	Name           string         // Unique identifier derived from directory name
	SourcePath     string         // Filesystem path to the function's source
	RuntimeID      string         // ID of the detected runtime (e.g., "go", "python")
	RuntimeHandler RuntimeHandler // Handler instance for runtime-specific operations
	ImageName      string         // Docker image name (e.g., "smolfaas-func-myapp:latest")
	ContainerName  string         // Container name (e.g., "smolfaas-func-myapp")
	InternalPort   int            // Port the function listens on (default: 8000)
	InvocationPath string         // Router path (e.g., "/invoke/myapp")

	// Build state tracking
	ContentHash     string    // SHA256 hash of all source files
	StoredHash      string    // Previously stored hash from SQLite
	StoredImageID   string    // Previously stored Docker image ID
	RequiresRebuild bool      // True if build is needed
	BuildSuccessful bool      // True if build completed successfully
	BuiltImageID    string    // Docker image ID after successful build
	LastBuildTime   time.Time // Timestamp of last successful build
}

// FunctionState represents the persisted state of a function in SQLite.
// This enables content-based rebuild detection instead of timestamp-based.
type FunctionState struct {
	Name          string    // Function name (primary key)
	ContentHash   string    // SHA256 hash of all source files
	ImageID       string    // Docker image ID
	LastBuildTime time.Time // When the function was last built
}

// ============================================================================
// Runtime Registry
// ============================================================================

// runtimeRegistry holds all registered RuntimeHandler implementations.
// Handlers are registered in init() and checked in priority order during detection.
var runtimeRegistry []RuntimeHandler

// registerRuntime adds a handler to the global registry.
// Called during init() to register all supported runtimes.
func registerRuntime(handler RuntimeHandler) {
	if handler == nil || handler.ID() == "" {
		logger.Error("Cannot register nil or ID-less RuntimeHandler")
		os.Exit(1)
	}
	logger.Debug("Registering runtime handler", "runtime", handler.ID())
	runtimeRegistry = append(runtimeRegistry, handler)
}

// ============================================================================
// Go Runtime Handler
// ============================================================================

// GoRuntimeHandler implements RuntimeHandler for Go functions.
// It produces minimal, statically-linked binaries using a multi-stage build:
//  1. Dependency download stage (cached separately)
//  2. Build stage with optimizations (-ldflags, -trimpath, CGO_ENABLED=0)
//  3. Minimal runtime stage (Alpine-based)
type GoRuntimeHandler struct{}

func (h *GoRuntimeHandler) ID() string      { return "go" }
func (h *GoRuntimeHandler) Version() string { return "1.24" }

func (h *GoRuntimeHandler) Detect(directoryPath string) bool {
	_, err := os.Stat(filepath.Join(directoryPath, "go.mod"))
	return err == nil
}

func (h *GoRuntimeHandler) GenerateLLB(funcDef FunctionDefinition) (llb.State, error) {
	logger.Info("Generating Go LLB", "function", funcDef.Name)

	goVersion := h.Version()
	builderImage := fmt.Sprintf("docker.io/library/golang:%s-alpine", goVersion)

	// Persistent caches for Go module and build artifacts
	goModCache := llb.AddMount("/go/pkg/mod", llb.Scratch(),
		llb.AsPersistentCacheDir(fmt.Sprintf("gomod-%s", goVersion), llb.CacheMountShared))
	goBuildCache := llb.AddMount("/root/.cache/go-build", llb.Scratch(),
		llb.AsPersistentCacheDir(fmt.Sprintf("gobuild-%s", goVersion), llb.CacheMountShared))

	// Source context reference
	srcPathRel, err := filepath.Rel(".", funcDef.SourcePath)
	if err != nil {
		srcPathRel = funcDef.SourcePath
	}
	localSourceOpts := []llb.LocalOption{
		llb.IncludePatterns([]string{"**/*"}),
		llb.ExcludePatterns([]string{"**/.*", "**/.git"}),
		llb.SharedKeyHint(srcPathRel),
		llb.LocalUniqueID(srcPathRel),
	}
	funcSource := llb.Local("context", localSourceOpts...)

	// Stage 1: Dependency Download
	// Copying only go.mod and go.sum first allows caching of downloaded modules
	// even when source code changes.
	dlStage := llb.Image(builderImage, llb.WithCustomName(fmt.Sprintf("Go %s Deps Stage", goVersion))).
		Dir("/app")
	dlStage = dlStage.File(llb.Copy(funcSource, "go.mod", "go.mod", &llb.CopyInfo{CreateDestPath: true}))
	// Copy go.sum if it exists (may not exist for projects with no dependencies)
	if _, err := os.Stat(filepath.Join(funcDef.SourcePath, "go.sum")); err == nil {
		dlStage = dlStage.File(llb.Copy(funcSource, "go.sum", "go.sum", &llb.CopyInfo{CreateDestPath: true}))
	}

	goPathEnv := llb.AddEnv("PATH", "/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	depsDownloadedState := dlStage.Run(
		llb.Shlex("go mod download"),
		goPathEnv,
		goModCache,
		llb.WithCustomName("Download Go Modules"),
	).Root()

	// Stage 2: Build
	// Start from deps state to ensure layer caching, then copy full source
	buildStage := depsDownloadedState.File(
		llb.Copy(funcSource, ".", "/app/", &llb.CopyInfo{CopyDirContentsOnly: true, CreateDestPath: true, AllowWildcard: true}),
		llb.WithCustomName("Copy full source"),
	)

	// Build with optimizations:
	// -ldflags='-s -w': Strip debug info and symbol table
	// -trimpath: Remove file system paths for reproducibility
	// CGO_ENABLED=0: Static binary, no C dependencies
	buildCmd := "go build -ldflags='-s -w' -trimpath -o /app/server ."
	buildResultState := buildStage.Run(
		llb.Shlex(buildCmd),
		goPathEnv,
		llb.AddEnv("CGO_ENABLED", "0"),
		llb.AddEnv("GOOS", "linux"),
		goModCache,
		goBuildCache,
		llb.WithCustomName("Build Go binary"),
	).Root()

	// Stage 3: Minimal Runtime
	// Use Alpine for small size while still having a shell for debugging
	finalStage := llb.Image("alpine:latest", llb.WithCustomName("Alpine Runtime")).
		Dir("/app")
	finalStage = finalStage.File(
		llb.Copy(buildResultState, "/app/server", "/app/server", &llb.CopyInfo{CreateDestPath: true}),
		llb.WithCustomName("Copy binary to runtime"),
	)

	return finalStage, nil
}

func (h *GoRuntimeHandler) Cmd() []string { return []string{"/app/server"} }

// ============================================================================
// Python Runtime Handler
// ============================================================================

// PythonRuntimeHandler implements RuntimeHandler for Python functions using uv.
// It uses a two-stage build for optimal caching:
//  1. Install dependencies only (cached when only source code changes)
//  2. Copy source and finalize
type PythonRuntimeHandler struct{}

func (h *PythonRuntimeHandler) ID() string      { return "python" }
func (h *PythonRuntimeHandler) Version() string { return "3.12" }

func (h *PythonRuntimeHandler) Detect(directoryPath string) bool {
	// Check for pyproject.toml (uv/poetry/modern Python)
	if _, err := os.Stat(filepath.Join(directoryPath, "pyproject.toml")); err == nil {
		return true
	}
	// Fallback to requirements.txt
	if _, err := os.Stat(filepath.Join(directoryPath, "requirements.txt")); err == nil {
		return true
	}
	return false
}

func (h *PythonRuntimeHandler) GenerateLLB(funcDef FunctionDefinition) (llb.State, error) {
	logger.Info("Generating Python LLB", "function", funcDef.Name)

	pythonVersion := h.Version()
	pythonImage := fmt.Sprintf("docker.io/library/python:%s-slim", pythonVersion)
	uvImage := "ghcr.io/astral-sh/uv:latest"

	// Persistent cache for uv package downloads
	uvCacheMount := llb.AddMount("/root/.cache/uv", llb.Scratch(),
		llb.AsPersistentCacheDir(fmt.Sprintf("uv-cache-%s", pythonVersion), llb.CacheMountShared))

	// Get uv binary from official image
	uvBase := llb.Image(uvImage, llb.WithCustomName("uv Base"))

	// Setup Python base with uv
	pythonBase := llb.Image(pythonImage, llb.WithCustomName(fmt.Sprintf("Python %s Slim", pythonVersion)))
	mode := fs.FileMode(0755)
	pythonWithUV := pythonBase.File(
		llb.Copy(uvBase, "/uv", "/usr/local/bin/uv", &llb.CopyInfo{CreateDestPath: true, Mode: &llb.ChmodOpt{Mode: mode}}),
		llb.WithCustomName("Copy uv binary"),
	).File(
		llb.Copy(uvBase, "/uvx", "/usr/local/bin/uvx", &llb.CopyInfo{CreateDestPath: true, Mode: &llb.ChmodOpt{Mode: mode}}),
		llb.WithCustomName("Copy uvx binary"),
	)

	// Source context
	srcPathRel, err := filepath.Rel(".", funcDef.SourcePath)
	if err != nil {
		srcPathRel = funcDef.SourcePath
	}
	localSourceOpts := []llb.LocalOption{
		llb.IncludePatterns([]string{"**/*"}),
		llb.ExcludePatterns([]string{"**/.*", "**/.git", "**/__pycache__", "**/*.pyc", "**/venv", "**/.venv"}),
		llb.SharedKeyHint(srcPathRel),
		llb.LocalUniqueID(srcPathRel),
	}
	funcSource := llb.Local("context", localSourceOpts...)

	// Check if using pyproject.toml or requirements.txt
	hasPyproject := false
	if _, err := os.Stat(filepath.Join(funcDef.SourcePath, "pyproject.toml")); err == nil {
		hasPyproject = true
	}

	buildStage := pythonWithUV.Dir("/app")

	if hasPyproject {
		// Stage 1: Copy dependency files only and install deps
		buildStage = buildStage.File(
			llb.Copy(funcSource, "pyproject.toml", "pyproject.toml", &llb.CopyInfo{CreateDestPath: true}),
		)
		// Check if uv.lock exists to determine install command
		hasUvLock := false
		if _, err := os.Stat(filepath.Join(funcDef.SourcePath, "uv.lock")); err == nil {
			hasUvLock = true
			buildStage = buildStage.File(
				llb.Copy(funcSource, "uv.lock", "uv.lock", &llb.CopyInfo{CreateDestPath: true}),
			)
		}

		// Install dependencies without the project itself
		var installCmd string
		if hasUvLock {
			installCmd = "uv sync --locked --no-install-project"
		} else {
			installCmd = "uv sync --no-install-project"
		}
		buildStage = buildStage.Run(
			llb.Shlex(installCmd),
			uvCacheMount,
			llb.WithCustomName("Install Python dependencies"),
		).Root()

		// Stage 2: Copy full source and finalize
		buildStage = buildStage.File(
			llb.Copy(funcSource, ".", "/app/", &llb.CopyInfo{CopyDirContentsOnly: true, CreateDestPath: true, AllowWildcard: true}),
			llb.WithCustomName("Copy full source"),
		)

		// Final sync to install the project
		var finalSyncCmd string
		if hasUvLock {
			finalSyncCmd = "uv sync --locked"
		} else {
			finalSyncCmd = "uv sync"
		}
		buildStage = buildStage.Run(
			llb.Shlex(finalSyncCmd),
			uvCacheMount,
			llb.WithCustomName("Install project"),
		).Root()
	} else {
		// requirements.txt workflow
		buildStage = buildStage.File(
			llb.Copy(funcSource, ".", "/app/", &llb.CopyInfo{CopyDirContentsOnly: true, CreateDestPath: true, AllowWildcard: true}),
			llb.WithCustomName("Copy full source"),
		)
		buildStage = buildStage.Run(
			llb.Shlex("uv pip install -r requirements.txt --system"),
			uvCacheMount,
			llb.WithCustomName("Install requirements"),
		).Root()
	}

	return buildStage, nil
}

func (h *PythonRuntimeHandler) Cmd() []string {
	return []string{"uv", "run", "main.py"}
}

// ============================================================================
// Node.js Runtime Handler
// ============================================================================

// NodeRuntimeHandler implements RuntimeHandler for Node.js functions.
// It supports npm, yarn, and pnpm package managers with appropriate caching.
type NodeRuntimeHandler struct{}

func (h *NodeRuntimeHandler) ID() string      { return "node" }
func (h *NodeRuntimeHandler) Version() string { return "22" }

func (h *NodeRuntimeHandler) Detect(directoryPath string) bool {
	_, err := os.Stat(filepath.Join(directoryPath, "package.json"))
	return err == nil
}

func (h *NodeRuntimeHandler) GenerateLLB(funcDef FunctionDefinition) (llb.State, error) {
	logger.Info("Generating Node.js LLB", "function", funcDef.Name)

	nodeVersion := h.Version()
	nodeImage := fmt.Sprintf("docker.io/library/node:%s-alpine", nodeVersion)

	// npm cache
	npmCacheMount := llb.AddMount("/root/.npm", llb.Scratch(),
		llb.AsPersistentCacheDir(fmt.Sprintf("npm-cache-%s", nodeVersion), llb.CacheMountShared))

	// Source context
	srcPathRel, err := filepath.Rel(".", funcDef.SourcePath)
	if err != nil {
		srcPathRel = funcDef.SourcePath
	}
	localSourceOpts := []llb.LocalOption{
		llb.IncludePatterns([]string{"**/*"}),
		llb.ExcludePatterns([]string{"**/.*", "**/.git", "**/node_modules"}),
		llb.SharedKeyHint(srcPathRel),
		llb.LocalUniqueID(srcPathRel),
	}
	funcSource := llb.Local("context", localSourceOpts...)

	// Detect package manager and lock files
	var installCmd string
	hasYarnLock := false
	hasPnpmLock := false
	hasNpmLock := false
	if _, err := os.Stat(filepath.Join(funcDef.SourcePath, "yarn.lock")); err == nil {
		hasYarnLock = true
	}
	if _, err := os.Stat(filepath.Join(funcDef.SourcePath, "pnpm-lock.yaml")); err == nil {
		hasPnpmLock = true
	}
	if _, err := os.Stat(filepath.Join(funcDef.SourcePath, "package-lock.json")); err == nil {
		hasNpmLock = true
	}

	if hasPnpmLock {
		installCmd = "corepack enable && pnpm install --frozen-lockfile"
	} else if hasYarnLock {
		installCmd = "corepack enable && yarn install --frozen-lockfile"
	} else if hasNpmLock {
		installCmd = "npm ci"
	} else {
		installCmd = "npm install"
	}

	// Stage 1: Install dependencies
	buildStage := llb.Image(nodeImage, llb.WithCustomName(fmt.Sprintf("Node.js %s Alpine", nodeVersion))).
		Dir("/app")

	// Copy package files first for caching
	buildStage = buildStage.File(
		llb.Copy(funcSource, "package.json", "package.json", &llb.CopyInfo{CreateDestPath: true}),
	)
	// Copy lock file if present
	if hasPnpmLock {
		buildStage = buildStage.File(
			llb.Copy(funcSource, "pnpm-lock.yaml", "pnpm-lock.yaml", &llb.CopyInfo{CreateDestPath: true}),
		)
	} else if hasYarnLock {
		buildStage = buildStage.File(
			llb.Copy(funcSource, "yarn.lock", "yarn.lock", &llb.CopyInfo{CreateDestPath: true}),
		)
	} else if hasNpmLock {
		buildStage = buildStage.File(
			llb.Copy(funcSource, "package-lock.json", "package-lock.json", &llb.CopyInfo{CreateDestPath: true}),
		)
	}

	buildStage = buildStage.Run(
		llb.Shlex(installCmd),
		npmCacheMount,
		llb.WithCustomName("Install Node.js dependencies"),
	).Root()

	// Stage 2: Copy source
	buildStage = buildStage.File(
		llb.Copy(funcSource, ".", "/app/", &llb.CopyInfo{CopyDirContentsOnly: true, CreateDestPath: true, AllowWildcard: true}),
		llb.WithCustomName("Copy full source"),
	)

	// Check if build script exists and run it
	if hasBuildScript(funcDef.SourcePath) {
		buildStage = buildStage.Run(
			llb.Shlex("npm run build"),
			llb.WithCustomName("Run build script"),
		).Root()
	}

	return buildStage, nil
}

func (h *NodeRuntimeHandler) Cmd() []string {
	return []string{"node", "index.js"}
}

// hasBuildScript checks if package.json has a build script
func hasBuildScript(sourcePath string) bool {
	pkgPath := filepath.Join(sourcePath, "package.json")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return false
	}
	var pkg map[string]interface{}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	if scripts, ok := pkg["scripts"].(map[string]interface{}); ok {
		_, hasBuild := scripts["build"]
		return hasBuild
	}
	return false
}

// ============================================================================
// ============================================================================
// Dockerfile Runtime Handler (Fallback)
// ============================================================================

// DockerfileRuntimeHandler implements RuntimeHandler for functions with a Dockerfile.
// This is the highest priority handler - if a function has a Dockerfile, we use it
// instead of the built-in runtime handlers.
type DockerfileRuntimeHandler struct{}

func (h *DockerfileRuntimeHandler) ID() string      { return "dockerfile" }
func (h *DockerfileRuntimeHandler) Version() string { return "custom" }

func (h *DockerfileRuntimeHandler) Detect(directoryPath string) bool {
	_, err := os.Stat(filepath.Join(directoryPath, "Dockerfile"))
	return err == nil
}

func (h *DockerfileRuntimeHandler) GenerateLLB(funcDef FunctionDefinition) (llb.State, error) {
	logger.Info("Generating Dockerfile LLB", "function", funcDef.Name)

	// Read Dockerfile content
	dockerfilePath := filepath.Join(funcDef.SourcePath, "Dockerfile")
	dockerfileContent, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return llb.Scratch(), fmt.Errorf("failed to read Dockerfile: %w", err)
	}

	// For Dockerfile-based builds, we use a simpler approach:
	// Build context with Dockerfile and let BuildKit handle it natively
	// This requires using the dockerfile frontend in the build call

	// Source context
	srcPathRel, err := filepath.Rel(".", funcDef.SourcePath)
	if err != nil {
		srcPathRel = funcDef.SourcePath
	}
	localSourceOpts := []llb.LocalOption{
		llb.IncludePatterns([]string{"**/*"}),
		llb.ExcludePatterns([]string{"**/.*", "**/.git"}),
		llb.SharedKeyHint(srcPathRel),
		llb.LocalUniqueID(srcPathRel),
	}
	funcSource := llb.Local("context", localSourceOpts...)

	// Create a state that copies the build context
	// The actual Dockerfile build will be handled by BuildKit's dockerfile frontend
	// For now, we create a marker state that indicates this needs Dockerfile build
	buildStage := llb.Scratch().File(
		llb.Copy(funcSource, ".", "/build-context/", &llb.CopyInfo{CopyDirContentsOnly: true, CreateDestPath: true, AllowWildcard: true}),
		llb.WithCustomName("Prepare Dockerfile build context"),
	)

	// Add Dockerfile content as metadata (this is a workaround)
	_ = dockerfileContent

	return buildStage, nil
}

func (h *DockerfileRuntimeHandler) Cmd() []string {
	// Default command - can be overridden by Dockerfile CMD
	return []string{"/app/server"}
}

// ============================================================================
// Hash Manager (SQLite-based State Persistence)
// ============================================================================

// HashManager handles content-based hashing and state persistence using SQLite.
// It computes SHA256 hashes of function source directories and stores them
// to enable incremental builds based on content changes rather than timestamps.
type HashManager struct {
	db     *sql.DB
	dbPath string
}

// NewHashManager creates a new HashManager with the specified database path.
func NewHashManager(dbPath string) (*HashManager, error) {
	logger.Info("Initializing HashManager", "path", dbPath)

	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Create schema
	schema := `
	CREATE TABLE IF NOT EXISTS function_state (
		name TEXT PRIMARY KEY,
		content_hash TEXT NOT NULL,
		image_id TEXT NOT NULL,
		last_build_time INTEGER NOT NULL
	);
	`
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create schema: %w", err)
	}

	return &HashManager{db: db, dbPath: dbPath}, nil
}

// Close closes the database connection.
func (hm *HashManager) Close() error {
	if hm.db != nil {
		return hm.db.Close()
	}
	return nil
}

// ComputeHash calculates a SHA256 hash of all files in the given directory.
// Files are sorted by path for deterministic hashing.
func (hm *HashManager) ComputeHash(dirPath string) (string, error) {
	hasher := sha256.New()

	var files []string
	err := filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Skip hidden files and directories
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Skip common build artifacts
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "target", "__pycache__", "venv", ".venv", "dist", "build":
				return filepath.SkipDir
			}
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to walk directory: %w", err)
	}

	// Sort for deterministic ordering
	sort.Strings(files)

	for _, filePath := range files {
		// Include relative path in hash
		relPath, err := filepath.Rel(dirPath, filePath)
		if err != nil {
			relPath = filePath
		}
		hasher.Write([]byte(relPath))
		hasher.Write([]byte{0}) // Separator

		// Include file content
		content, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("failed to read file %s: %w", filePath, err)
		}
		hasher.Write(content)
		hasher.Write([]byte{0}) // Separator
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// GetState retrieves the stored state for a function.
func (hm *HashManager) GetState(funcName string) (*FunctionState, error) {
	row := hm.db.QueryRow(
		"SELECT name, content_hash, image_id, last_build_time FROM function_state WHERE name = ?",
		funcName,
	)

	var state FunctionState
	var lastBuildUnix int64
	err := row.Scan(&state.Name, &state.ContentHash, &state.ImageID, &lastBuildUnix)
	if err == sql.ErrNoRows {
		return nil, nil // Not found
	}
	if err != nil {
		return nil, fmt.Errorf("failed to query state: %w", err)
	}

	state.LastBuildTime = time.Unix(lastBuildUnix, 0)
	return &state, nil
}

// SaveState persists the state for a function.
func (hm *HashManager) SaveState(state FunctionState) error {
	_, err := hm.db.Exec(
		`INSERT OR REPLACE INTO function_state (name, content_hash, image_id, last_build_time)
		 VALUES (?, ?, ?, ?)`,
		state.Name, state.ContentHash, state.ImageID, state.LastBuildTime.Unix(),
	)
	if err != nil {
		return fmt.Errorf("failed to save state: %w", err)
	}
	return nil
}

// DeleteState removes the state for a function.
func (hm *HashManager) DeleteState(funcName string) error {
	_, err := hm.db.Exec("DELETE FROM function_state WHERE name = ?", funcName)
	if err != nil {
		return fmt.Errorf("failed to delete state: %w", err)
	}
	return nil
}

// GetAllStates retrieves all stored function states.
func (hm *HashManager) GetAllStates() ([]FunctionState, error) {
	rows, err := hm.db.Query("SELECT name, content_hash, image_id, last_build_time FROM function_state")
	if err != nil {
		return nil, fmt.Errorf("failed to query states: %w", err)
	}
	defer rows.Close()

	var states []FunctionState
	for rows.Next() {
		var state FunctionState
		var lastBuildUnix int64
		if err := rows.Scan(&state.Name, &state.ContentHash, &state.ImageID, &lastBuildUnix); err != nil {
			return nil, fmt.Errorf("failed to scan state: %w", err)
		}
		state.LastBuildTime = time.Unix(lastBuildUnix, 0)
		states = append(states, state)
	}

	return states, rows.Err()
}

// ============================================================================
// BuildKit Manager
// ============================================================================

// BuildkitManager handles the lifecycle of the BuildKit daemon container.
// It manages starting/stopping the daemon and maintaining a client connection.
type BuildkitManager struct {
	dockerClient    *docker.Client
	buildkitClient  *buildkit.Client
	ctx             context.Context
	buildkitImage   string
	containerName   string
	cacheVolumeName string
}

// NewBuildkitManager creates a new BuildkitManager instance.
func NewBuildkitManager(ctx context.Context, dockerClient *docker.Client, containerName, cacheVolumeName string) (*BuildkitManager, error) {
	if dockerClient == nil {
		return nil, fmt.Errorf("docker client cannot be nil")
	}
	logger.Info("Initializing BuildkitManager", "container", containerName, "cache", cacheVolumeName)
	return &BuildkitManager{
		dockerClient:    dockerClient,
		ctx:             ctx,
		buildkitImage:   "moby/buildkit:latest",
		containerName:   containerName,
		cacheVolumeName: cacheVolumeName,
	}, nil
}

// ensureBuildkitRunning ensures the BuildKit daemon container is running.
func (bm *BuildkitManager) ensureBuildkitRunning() error {
	logger.Info("Ensuring BuildKit daemon is running", "container", bm.containerName)

	// Check if container exists and is running
	contJSON, err := bm.dockerClient.ContainerInspect(bm.ctx, bm.containerName)
	if err == nil {
		if contJSON.State.Running {
			logger.Info("BuildKit container already running")
			return nil
		}
		// Container exists but not running - remove it
		logger.Info("Removing stopped BuildKit container")
		if err := bm.dockerClient.ContainerRemove(bm.ctx, bm.containerName, container.RemoveOptions{Force: true}); err != nil {
			logger.Warn("Failed to remove existing container", "error", err)
		}
	} else if !docker.IsErrNotFound(err) {
		return fmt.Errorf("failed to inspect container: %w", err)
	}

	// Ensure cache volume exists
	logger.Info("Ensuring cache volume exists", "volume", bm.cacheVolumeName)
	_, err = bm.dockerClient.VolumeCreate(bm.ctx, volume.CreateOptions{Name: bm.cacheVolumeName})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		return fmt.Errorf("failed to create cache volume: %w", err)
	}

	// Ensure BuildKit image exists
	_, err = bm.dockerClient.ImageInspect(bm.ctx, bm.buildkitImage)
	if err != nil {
		if docker.IsErrNotFound(err) {
			logger.Info("Pulling BuildKit image", "image", bm.buildkitImage)
			reader, err := bm.dockerClient.ImagePull(bm.ctx, bm.buildkitImage, image.PullOptions{})
			if err != nil {
				return fmt.Errorf("failed to pull BuildKit image: %w", err)
			}
			defer reader.Close()
			io.Copy(os.Stdout, reader)
		} else {
			return fmt.Errorf("failed to inspect BuildKit image: %w", err)
		}
	}

	// Create container
	logger.Info("Creating BuildKit container")
	containerConfig := &container.Config{
		Image: bm.buildkitImage,
	}
	hostConfig := &container.HostConfig{
		Privileged: true,
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: bm.cacheVolumeName,
				Target: "/var/lib/buildkit",
			},
		},
	}

	resp, err := bm.dockerClient.ContainerCreate(bm.ctx, containerConfig, hostConfig, nil, nil, bm.containerName)
	if err != nil {
		return fmt.Errorf("failed to create container: %w", err)
	}

	// Start container
	logger.Info("Starting BuildKit container", "id", resp.ID[:12])
	if err := bm.dockerClient.ContainerStart(bm.ctx, resp.ID, container.StartOptions{}); err != nil {
		bm.dockerClient.ContainerRemove(bm.ctx, resp.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("failed to start container: %w", err)
	}

	// Wait for daemon to initialize
	time.Sleep(time.Second)
	logger.Info("BuildKit daemon started")
	return nil
}

// connectToBuildkit establishes a client connection to the BuildKit daemon.
func (bm *BuildkitManager) connectToBuildkit() error {
	logger.Info("Connecting to BuildKit daemon")
	if bm.buildkitClient != nil {
		bm.buildkitClient.Close()
		bm.buildkitClient = nil
	}

	buildkitHost := fmt.Sprintf("docker-container://%s", bm.containerName)
	client, err := buildkit.New(bm.ctx, buildkitHost)
	if err != nil {
		return fmt.Errorf("failed to connect to BuildKit: %w", err)
	}

	bm.buildkitClient = client
	logger.Info("Connected to BuildKit daemon")
	return nil
}

// shutdown closes the BuildKit client connection.
func (bm *BuildkitManager) shutdown() error {
	if bm.buildkitClient != nil {
		logger.Info("Closing BuildKit client connection")
		err := bm.buildkitClient.Close()
		bm.buildkitClient = nil
		return err
	}
	return nil
}

// StopAndRemoveDaemon stops and removes the BuildKit daemon container.
func (bm *BuildkitManager) StopAndRemoveDaemon() error {
	logger.Info("Stopping BuildKit daemon", "container", bm.containerName)

	contJSON, err := bm.dockerClient.ContainerInspect(bm.ctx, bm.containerName)
	if err != nil {
		if docker.IsErrNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to inspect container: %w", err)
	}

	if contJSON.State.Running {
		timeout := 10
		if err := bm.dockerClient.ContainerStop(bm.ctx, contJSON.ID, container.StopOptions{Timeout: &timeout}); err != nil {
			logger.Warn("Failed to stop container gracefully", "error", err)
		}
	}

	if err := bm.dockerClient.ContainerRemove(bm.ctx, contJSON.ID, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	logger.Info("BuildKit daemon stopped and removed")
	return nil
}

// ============================================================================
// FaaS Builder
// ============================================================================

// FaaSBuilder orchestrates function discovery and building.
// It uses the HashManager for incremental builds based on content hashing.
type FaaSBuilder struct {
	buildkitMgr *BuildkitManager
	hashMgr     *HashManager
	imagePrefix string
}

// NewFaaSBuilder creates a new FaaSBuilder instance.
func NewFaaSBuilder(bm *BuildkitManager, hm *HashManager, imagePrefix string) *FaaSBuilder {
	logger.Info("Initializing FaaSBuilder")
	return &FaaSBuilder{buildkitMgr: bm, hashMgr: hm, imagePrefix: imagePrefix}
}

// discoverFunctions scans the base directory for function subdirectories.
func (fb *FaaSBuilder) discoverFunctions(baseDir string) ([]FunctionDefinition, error) {
	logger.Info("Discovering functions", "dir", baseDir, "handlers", len(runtimeRegistry))

	entries, err := os.ReadDir(baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read functions directory: %w", err)
	}

	var funcs []FunctionDefinition
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		funcName := entry.Name()
		funcPath := filepath.Join(baseDir, funcName)

		// Check each runtime handler in priority order
		// Dockerfile > Go > Rust > Node > Python
		var detectedHandler RuntimeHandler
		for _, handler := range runtimeRegistry {
			if handler.Detect(funcPath) {
				logger.Info("Detected function", "name", funcName, "runtime", handler.ID())
				detectedHandler = handler
				break
			}
		}

		if detectedHandler != nil {
			funcs = append(funcs, FunctionDefinition{
				Name:           funcName,
				SourcePath:     funcPath,
				RuntimeID:      detectedHandler.ID(),
				RuntimeHandler: detectedHandler,
				ImageName:      fmt.Sprintf("%s-%s:latest", fb.imagePrefix, funcName),
				ContainerName:  fmt.Sprintf("%s-%s", fb.imagePrefix, funcName),
				InternalPort:   8000,
				InvocationPath: fmt.Sprintf("/invoke/%s", funcName),
			})
		}
	}

	logger.Info("Discovery complete", "found", len(funcs))
	return funcs, nil
}

// checkFunctionIncrementalStatus determines if a function needs rebuilding.
func (fb *FaaSBuilder) checkFunctionIncrementalStatus(funcDef *FunctionDefinition) error {
	logger.Info("Checking build status", "function", funcDef.Name)

	// Compute current content hash
	currentHash, err := fb.hashMgr.ComputeHash(funcDef.SourcePath)
	if err != nil {
		return fmt.Errorf("failed to compute hash: %w", err)
	}
	funcDef.ContentHash = currentHash

	// Get stored state
	state, err := fb.hashMgr.GetState(funcDef.Name)
	if err != nil {
		return fmt.Errorf("failed to get stored state: %w", err)
	}

	if state == nil {
		// No previous state - rebuild needed
		logger.Info("No previous state found", "function", funcDef.Name)
		funcDef.RequiresRebuild = true
		return nil
	}

	funcDef.StoredHash = state.ContentHash
	funcDef.StoredImageID = state.ImageID
	funcDef.LastBuildTime = state.LastBuildTime

	// Check if hash changed
	if currentHash != state.ContentHash {
		logger.Info("Content hash changed", "function", funcDef.Name,
			"old", state.ContentHash[:16], "new", currentHash[:16])
		funcDef.RequiresRebuild = true
		return nil
	}

	// Check if image still exists
	imgInspect, err := fb.buildkitMgr.dockerClient.ImageInspect(fb.buildkitMgr.ctx, funcDef.ImageName)
	if err != nil {
		if docker.IsErrNotFound(err) {
			logger.Info("Image not found, rebuild needed", "function", funcDef.Name)
			funcDef.RequiresRebuild = true
			return nil
		}
		return fmt.Errorf("failed to inspect image: %w", err)
	}

	// Check if image ID matches stored ID
	if imgInspect.ID != state.ImageID {
		logger.Info("Image ID mismatch, rebuild needed", "function", funcDef.Name)
		funcDef.RequiresRebuild = true
		return nil
	}

	logger.Info("Function is up-to-date", "function", funcDef.Name)
	funcDef.RequiresRebuild = false
	return nil
}

// buildAndLoadFunctionImage builds a function image and loads it into Docker.
func (fb *FaaSBuilder) buildAndLoadFunctionImage(def *llb.Definition, funcDef *FunctionDefinition) error {
	if fb.buildkitMgr == nil || fb.buildkitMgr.buildkitClient == nil {
		return fmt.Errorf("buildkit client not available")
	}

	logger.Info("Building function image", "function", funcDef.Name, "image", funcDef.ImageName)
	startTime := time.Now()
	ctx := fb.buildkitMgr.ctx

	pr, pw := io.Pipe()
	var tarBuffer bytes.Buffer
	var tarWg sync.WaitGroup
	tarWg.Add(1)

	exportEntry := buildkit.ExportEntry{
		Type:  buildkit.ExporterDocker,
		Attrs: map[string]string{"name": funcDef.ImageName},
		Output: func(map[string]string) (io.WriteCloser, error) {
			return pw, nil
		},
	}
	solveOpt := buildkit.SolveOpt{
		Exports:   []buildkit.ExportEntry{exportEntry},
		LocalDirs: map[string]string{"context": funcDef.SourcePath},
	}

	buildEg, buildCtx := errgroup.WithContext(ctx)
	ch := make(chan *buildkit.SolveStatus)

	// Tar reader goroutine
	buildEg.Go(func() error {
		defer tarWg.Done()
		defer pr.Close()
		_, err := io.Copy(&tarBuffer, pr)
		if err != nil && err != io.ErrClosedPipe {
			return fmt.Errorf("tar read error: %w", err)
		}
		return nil
	})

	// Progress display goroutine
	buildEg.Go(func() error {
		display, err := progressui.NewDisplay(os.Stderr, progressui.AutoMode)
		if err != nil {
			// Drain channel if display fails
			go func() {
				for range ch {
				}
			}()
			return nil
		}
		_, err = display.UpdateFrom(buildCtx, ch)
		return err
	})

	// Build goroutine
	buildEg.Go(func() error {
		defer pw.Close()
		_, err := fb.buildkitMgr.buildkitClient.Build(buildCtx, solveOpt, "",
			func(ctx context.Context, c bkgw.Client) (*bkgw.Result, error) {
				res, err := c.Solve(ctx, bkgw.SolveRequest{Definition: def.ToPB()})
				if err != nil {
					return nil, fmt.Errorf("solve failed: %w", err)
				}
				return res, nil
			},
			ch,
		)
		return err
	})

	if err := buildEg.Wait(); err != nil {
		return fmt.Errorf("build failed: %w", err)
	}

	pw.Close()
	tarWg.Wait()

	// Load image into Docker
	logger.Info("Loading image into Docker", "size", tarBuffer.Len())
	resp, err := fb.buildkitMgr.dockerClient.ImageLoad(ctx, bytes.NewReader(tarBuffer.Bytes()))
	if err != nil {
		return fmt.Errorf("failed to load image: %w", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	// Get image ID
	imgInspect, err := fb.buildkitMgr.dockerClient.ImageInspect(ctx, funcDef.ImageName)
	if err != nil {
		return fmt.Errorf("failed to inspect built image: %w", err)
	}

	funcDef.BuildSuccessful = true
	funcDef.BuiltImageID = imgInspect.ID
	funcDef.LastBuildTime = time.Now()

	// Save state to SQLite
	if err := fb.hashMgr.SaveState(FunctionState{
		Name:          funcDef.Name,
		ContentHash:   funcDef.ContentHash,
		ImageID:       imgInspect.ID,
		LastBuildTime: funcDef.LastBuildTime,
	}); err != nil {
		logger.Warn("Failed to save state", "error", err)
	}

	logger.Info("Image built successfully", "function", funcDef.Name,
		"duration", time.Since(startTime).Round(time.Millisecond))
	return nil
}

// BuildAndCheckAllFunctions orchestrates the build process for all functions.
func (fb *FaaSBuilder) BuildAndCheckAllFunctions(baseDir string) ([]FunctionDefinition, error) {
	logger.Info("Starting build process", "dir", baseDir)

	funcs, err := fb.discoverFunctions(baseDir)
	if err != nil {
		return nil, err
	}
	if len(funcs) == 0 {
		logger.Info("No functions found")
		return nil, nil
	}

	var readyFuncs []FunctionDefinition
	var readyMutex sync.Mutex
	var buildErrors []error
	var errorMutex sync.Mutex

	g, groupCtx := errgroup.WithContext(fb.buildkitMgr.ctx)

	for i := range funcs {
		fnIndex := i
		fn := &funcs[fnIndex]

		g.Go(func() error {
			if groupCtx.Err() != nil {
				return groupCtx.Err()
			}

			logger.Info("Processing function", "name", fn.Name)

			if err := fb.checkFunctionIncrementalStatus(fn); err != nil {
				errorMutex.Lock()
				buildErrors = append(buildErrors, fmt.Errorf("%s: %w", fn.Name, err))
				errorMutex.Unlock()
				return nil
			}

			if fn.RequiresRebuild {
				logger.Info("Rebuilding function", "name", fn.Name)

				llbState, err := fn.RuntimeHandler.GenerateLLB(*fn)
				if err != nil {
					errorMutex.Lock()
					buildErrors = append(buildErrors, fmt.Errorf("%s LLB: %w", fn.Name, err))
					errorMutex.Unlock()
					return nil
				}

				def, err := llbState.Marshal(groupCtx)
				if err != nil {
					errorMutex.Lock()
					buildErrors = append(buildErrors, fmt.Errorf("%s marshal: %w", fn.Name, err))
					errorMutex.Unlock()
					return nil
				}

				if err := fb.buildAndLoadFunctionImage(def, fn); err != nil {
					errorMutex.Lock()
					buildErrors = append(buildErrors, fmt.Errorf("%s build: %w", fn.Name, err))
					errorMutex.Unlock()
					return nil
				}
			} else {
				fn.BuildSuccessful = true
			}

			if fn.BuildSuccessful {
				readyMutex.Lock()
				readyFuncs = append(readyFuncs, *fn)
				readyMutex.Unlock()
			}

			return nil
		})
	}

	waitErr := g.Wait()

	if waitErr != nil {
		return readyFuncs, waitErr
	}
	if len(buildErrors) > 0 {
		return readyFuncs, fmt.Errorf("build errors: %v", buildErrors)
	}

	logger.Info("Build process complete", "ready", len(readyFuncs))
	return readyFuncs, nil
}

// ============================================================================
// Function Container Manager
// ============================================================================

// FunctionManager handles the lifecycle of function containers.
type FunctionManager struct {
	dockerClient *docker.Client
	ctx          context.Context
	networkName  string
}

// NewFunctionManager creates a new FunctionManager instance.
func NewFunctionManager(ctx context.Context, dockerClient *docker.Client, networkName string) (*FunctionManager, error) {
	if dockerClient == nil {
		return nil, fmt.Errorf("docker client cannot be nil")
	}
	if networkName == "" {
		return nil, fmt.Errorf("network name cannot be empty")
	}
	logger.Info("Initializing FunctionManager", "network", networkName)
	return &FunctionManager{dockerClient: dockerClient, ctx: ctx, networkName: networkName}, nil
}

// StartAll starts containers for all provided functions concurrently.
func (fm *FunctionManager) StartAll(functions []FunctionDefinition) error {
	logger.Info("Starting function containers", "count", len(functions))
	if len(functions) == 0 {
		return nil
	}

	g, groupCtx := errgroup.WithContext(fm.ctx)

	for _, fn := range functions {
		funcDef := fn

		g.Go(func() error {
			logger.Info("Starting container", "name", funcDef.ContainerName)

			// Check if container exists
			contInspect, err := fm.dockerClient.ContainerInspect(groupCtx, funcDef.ContainerName)
			if err == nil {
				if contInspect.State.Running {
					logger.Info("Container already running", "name", funcDef.ContainerName)
					return nil
				}
				// Remove stopped container
				if err := fm.dockerClient.ContainerRemove(groupCtx, contInspect.ID, container.RemoveOptions{Force: true}); err != nil {
					return fmt.Errorf("failed to remove existing container: %w", err)
				}
			} else if !docker.IsErrNotFound(err) {
				return fmt.Errorf("failed to inspect container: %w", err)
			}

			// Create container configuration
			exposedPort, _ := nat.NewPort("tcp", fmt.Sprintf("%d", funcDef.InternalPort))

			containerCmd := funcDef.RuntimeHandler.Cmd()
			containerEnv := []string{
				fmt.Sprintf("PORT=%d", funcDef.InternalPort),
			}

			containerCfg := &container.Config{
				Image:        funcDef.ImageName,
				WorkingDir:   "/app",
				Cmd:          containerCmd,
				Env:          containerEnv,
				ExposedPorts: nat.PortSet{exposedPort: struct{}{}},
				Labels: map[string]string{
					"com.smolfaas.function": funcDef.Name,
					"com.smolfaas.managed":  "true",
				},
			}

			hostCfg := &container.HostConfig{
				RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			}

			networkingCfg := &network.NetworkingConfig{
				EndpointsConfig: map[string]*network.EndpointSettings{
					fm.networkName: {},
				},
			}

			// Create and start container
			resp, err := fm.dockerClient.ContainerCreate(groupCtx, containerCfg, hostCfg, networkingCfg, nil, funcDef.ContainerName)
			if err != nil {
				return fmt.Errorf("failed to create container: %w", err)
			}

			if err := fm.dockerClient.ContainerStart(groupCtx, resp.ID, container.StartOptions{}); err != nil {
				fm.dockerClient.ContainerRemove(groupCtx, resp.ID, container.RemoveOptions{Force: true})
				return fmt.Errorf("failed to start container: %w", err)
			}

			logger.Info("Container started", "name", funcDef.ContainerName, "id", resp.ID[:12])
			return nil
		})
	}

	return g.Wait()
}

// StopAndRemove stops and removes a container by name.
func (fm *FunctionManager) StopAndRemove(containerName string) error {
	logger.Info("Stopping container", "name", containerName)

	contJSON, err := fm.dockerClient.ContainerInspect(fm.ctx, containerName)
	if err != nil {
		if docker.IsErrNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to inspect container: %w", err)
	}

	if contJSON.State.Running {
		if err := fm.dockerClient.ContainerStop(fm.ctx, containerName, container.StopOptions{}); err != nil {
			logger.Warn("Failed to stop container", "error", err)
		}
	}

	if err := fm.dockerClient.ContainerRemove(fm.ctx, containerName, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	logger.Info("Container removed", "name", containerName)
	return nil
}

// StopAllByPrefix stops and removes all containers matching the prefix.
func (fm *FunctionManager) StopAllByPrefix(prefix string) error {
	logger.Info("Stopping containers with prefix", "prefix", prefix)

	containers, err := fm.dockerClient.ContainerList(fm.ctx, container.ListOptions{All: true})
	if err != nil {
		return fmt.Errorf("failed to list containers: %w", err)
	}

	for _, cont := range containers {
		for _, name := range cont.Names {
			cleanedName := strings.TrimPrefix(name, "/")
			if strings.HasPrefix(cleanedName, prefix) {
				if err := fm.StopAndRemove(cont.ID); err != nil {
					logger.Warn("Failed to stop container", "name", cleanedName, "error", err)
				}
				break
			}
		}
	}

	return nil
}

// RemoveImage removes a Docker image.
func (fm *FunctionManager) RemoveImage(imageName string) error {
	logger.Info("Removing image", "name", imageName)

	_, err := fm.dockerClient.ImageRemove(fm.ctx, imageName, image.RemoveOptions{Force: true, PruneChildren: true})
	if err != nil {
		if docker.IsErrNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to remove image: %w", err)
	}

	return nil
}

// ============================================================================
// Caddy Router Manager
// ============================================================================

// CaddyManager handles the Caddy reverse proxy container.
// It builds a custom Caddy image via LLB with the Caddyfile embedded.
type CaddyManager struct {
	dockerClient      *docker.Client
	ctx               context.Context
	containerName     string
	networkName       string
	adminAPIAddr      string
	caddyDataVolume   string
	caddyConfigVolume string
	caddyImageName    string
	routerImageName   string
}

// NewCaddyManager creates a new CaddyManager instance.
func NewCaddyManager(ctx context.Context, dockerClient *docker.Client, containerName, networkName string) (*CaddyManager, error) {
	if dockerClient == nil {
		return nil, fmt.Errorf("docker client cannot be nil")
	}
	logger.Info("Initializing CaddyManager", "container", containerName, "network", networkName)
	return &CaddyManager{
		dockerClient:      dockerClient,
		ctx:               ctx,
		containerName:     containerName,
		networkName:       networkName,
		adminAPIAddr:      "localhost:2019",
		caddyDataVolume:   containerName + "-data",
		caddyConfigVolume: containerName + "-config",
		caddyImageName:    "caddy:latest",
		routerImageName:   containerName + ":latest",
	}, nil
}

// GenerateCaddyLLB creates the LLB for building a custom Caddy image.
func (cm *CaddyManager) GenerateCaddyLLB(hostname, adminAddr string) (llb.State, error) {
	logger.Info("Generating Caddy LLB", "hostname", hostname)

	caddyfileContent := fmt.Sprintf(`{
    admin %s
}

%s {
    log {
        output stdout
        format console
    }

    route /health {
        respond "SmolFaaS Router OK" 200
    }
}
`, adminAddr, hostname)

	caddyBase := llb.Image(cm.caddyImageName, llb.WithCustomName("Caddy Base"))

	// Create Caddyfile
	caddyfileState := llb.Scratch().File(
		llb.Mkfile("/Caddyfile", 0644, []byte(caddyfileContent)),
		llb.WithCustomName("Create Caddyfile"),
	)

	// Copy Caddyfile into Caddy image
	withCaddyfile := caddyBase.File(
		llb.Copy(caddyfileState, "/Caddyfile", "/etc/caddy/Caddyfile", &llb.CopyInfo{CreateDestPath: true}),
		llb.WithCustomName("Copy Caddyfile"),
	)

	// Install curl for admin API calls
	finalState := withCaddyfile.Run(
		llb.Shlex("apk add --no-cache curl"),
		llb.WithCustomName("Install curl"),
	).Root()

	return finalState, nil
}

// BuildRouterImage builds the custom Caddy router image.
func (cm *CaddyManager) BuildRouterImage(buildkitMgr *BuildkitManager, hostname string) error {
	logger.Info("Building Caddy router image")

	llbState, err := cm.GenerateCaddyLLB(hostname, cm.adminAPIAddr)
	if err != nil {
		return fmt.Errorf("failed to generate Caddy LLB: %w", err)
	}

	def, err := llbState.Marshal(cm.ctx)
	if err != nil {
		return fmt.Errorf("failed to marshal LLB: %w", err)
	}

	ctx := cm.ctx
	pr, pw := io.Pipe()
	var tarBuffer bytes.Buffer
	var tarWg sync.WaitGroup
	tarWg.Add(1)

	exportEntry := buildkit.ExportEntry{
		Type:  buildkit.ExporterDocker,
		Attrs: map[string]string{"name": cm.routerImageName},
		Output: func(map[string]string) (io.WriteCloser, error) {
			return pw, nil
		},
	}
	solveOpt := buildkit.SolveOpt{
		Exports: []buildkit.ExportEntry{exportEntry},
	}

	buildEg, buildCtx := errgroup.WithContext(ctx)
	ch := make(chan *buildkit.SolveStatus)

	buildEg.Go(func() error {
		defer tarWg.Done()
		defer pr.Close()
		_, err := io.Copy(&tarBuffer, pr)
		if err != nil && err != io.ErrClosedPipe {
			return err
		}
		return nil
	})

	buildEg.Go(func() error {
		display, err := progressui.NewDisplay(os.Stderr, progressui.AutoMode)
		if err != nil {
			go func() {
				for range ch {
				}
			}()
			return nil
		}
		_, err = display.UpdateFrom(buildCtx, ch)
		return err
	})

	buildEg.Go(func() error {
		defer pw.Close()
		_, err := buildkitMgr.buildkitClient.Build(buildCtx, solveOpt, "",
			func(ctx context.Context, c bkgw.Client) (*bkgw.Result, error) {
				res, err := c.Solve(ctx, bkgw.SolveRequest{Definition: def.ToPB()})
				if err != nil {
					return nil, err
				}
				return res, nil
			},
			ch,
		)
		return err
	})

	if err := buildEg.Wait(); err != nil {
		return fmt.Errorf("failed to build Caddy image: %w", err)
	}

	pw.Close()
	tarWg.Wait()

	resp, err := cm.dockerClient.ImageLoad(ctx, bytes.NewReader(tarBuffer.Bytes()))
	if err != nil {
		return fmt.Errorf("failed to load Caddy image: %w", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	logger.Info("Caddy router image built", "image", cm.routerImageName)
	return nil
}

// execInCaddyContainer executes a command inside the Caddy container.
func (cm *CaddyManager) execInCaddyContainer(cmdArgs ...string) (string, string, error) {
	execConfig := container.ExecOptions{
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmdArgs,
	}

	execIDResp, err := cm.dockerClient.ContainerExecCreate(cm.ctx, cm.containerName, execConfig)
	if err != nil {
		return "", "", fmt.Errorf("failed to create exec: %w", err)
	}

	hijackedResp, err := cm.dockerClient.ContainerExecAttach(cm.ctx, execIDResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", "", fmt.Errorf("failed to attach to exec: %w", err)
	}
	defer hijackedResp.Close()

	var stdoutBuf, stderrBuf bytes.Buffer
	stdcopy.StdCopy(&stdoutBuf, &stderrBuf, hijackedResp.Reader)

	time.Sleep(100 * time.Millisecond)
	inspectResp, err := cm.dockerClient.ContainerExecInspect(cm.ctx, execIDResp.ID)
	if err != nil {
		return stdoutBuf.String(), stderrBuf.String(), err
	}

	if inspectResp.ExitCode != 0 {
		return stdoutBuf.String(), stderrBuf.String(),
			fmt.Errorf("command exited with code %d: %s", inspectResp.ExitCode, stderrBuf.String())
	}

	return strings.TrimSpace(stdoutBuf.String()), strings.TrimSpace(stderrBuf.String()), nil
}

// Start starts the Caddy router container.
func (cm *CaddyManager) Start() error {
	logger.Info("Starting Caddy router container", "name", cm.containerName)

	// Check if container already exists and is running
	contJSON, err := cm.dockerClient.ContainerInspect(cm.ctx, cm.containerName)
	if err == nil {
		if contJSON.State.Running {
			logger.Info("Caddy container already running")
			return nil
		}
		// Remove stopped container
		timeout := 5
		cm.dockerClient.ContainerStop(cm.ctx, cm.containerName, container.StopOptions{Timeout: &timeout})
		cm.dockerClient.ContainerRemove(cm.ctx, cm.containerName, container.RemoveOptions{Force: true})
	} else if !docker.IsErrNotFound(err) {
		return fmt.Errorf("failed to inspect container: %w", err)
	}

	// Ensure volumes exist
	for _, vol := range []string{cm.caddyDataVolume, cm.caddyConfigVolume} {
		_, err := cm.dockerClient.VolumeCreate(cm.ctx, volume.CreateOptions{Name: vol})
		if err != nil && !strings.Contains(err.Error(), "already exists") {
			return fmt.Errorf("failed to create volume %s: %w", vol, err)
		}
	}

	// Port mappings
	httpPort := nat.Port("80/tcp")
	httpsPort := nat.Port("443/tcp")

	containerCfg := &container.Config{
		Image:        cm.routerImageName,
		ExposedPorts: nat.PortSet{httpPort: {}, httpsPort: {}},
		Cmd:          []string{"caddy", "run", "--config", "/etc/caddy/Caddyfile", "--adapter", "caddyfile"},
		Labels: map[string]string{
			"com.smolfaas.router":  "true",
			"com.smolfaas.managed": "true",
		},
	}

	hostCfg := &container.HostConfig{
		PortBindings: nat.PortMap{
			httpPort:  []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "80"}},
			httpsPort: []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "443"}},
		},
		Mounts: []mount.Mount{
			{Type: mount.TypeVolume, Source: cm.caddyDataVolume, Target: "/data"},
			{Type: mount.TypeVolume, Source: cm.caddyConfigVolume, Target: "/config"},
		},
		RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
	}

	networkingCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			cm.networkName: {},
		},
	}

	resp, err := cm.dockerClient.ContainerCreate(cm.ctx, containerCfg, hostCfg, networkingCfg, nil, cm.containerName)
	if err != nil {
		return fmt.Errorf("failed to create Caddy container: %w", err)
	}

	if err := cm.dockerClient.ContainerStart(cm.ctx, resp.ID, container.StartOptions{}); err != nil {
		cm.dockerClient.ContainerRemove(cm.ctx, resp.ID, container.RemoveOptions{Force: true})
		return fmt.Errorf("failed to start Caddy container: %w", err)
	}

	// Wait for Caddy to initialize
	time.Sleep(3 * time.Second)
	logger.Info("Caddy router started", "id", resp.ID[:12])
	return nil
}

// CaddyRoute represents a route configuration for Caddy's admin API.
type CaddyRoute struct {
	ID       string           `json:"@id,omitempty"`
	Match    []CaddyMatcher   `json:"match,omitempty"`
	Handle   []map[string]any `json:"handle"`
	Terminal bool             `json:"terminal,omitempty"`
}

// CaddyMatcher defines route matching criteria.
type CaddyMatcher struct {
	Path []string `json:"path"`
}

// AddFunctionRoute adds a reverse proxy route for a function.
func (cm *CaddyManager) AddFunctionRoute(funcDef FunctionDefinition) error {
	routeID := "smolfaas-func-" + funcDef.Name
	logger.Info("Adding Caddy route", "function", funcDef.Name, "path", funcDef.InvocationPath)

	route := CaddyRoute{
		ID: routeID,
		Match: []CaddyMatcher{
			{Path: []string{funcDef.InvocationPath}},
		},
		Handle: []map[string]any{
			{
				"handler": "rewrite",
				"uri":     "/",
			},
			{
				"handler": "reverse_proxy",
				"upstreams": []map[string]string{
					{"dial": fmt.Sprintf("%s:%d", funcDef.ContainerName, funcDef.InternalPort)},
				},
			},
		},
		Terminal: true,
	}

	jsonData, err := json.Marshal(route)
	if err != nil {
		return fmt.Errorf("failed to marshal route: %w", err)
	}

	apiPath := "/config/apps/http/servers/srv0/routes/0/handle/0/routes"
	fullAPIURL := fmt.Sprintf("http://%s%s", cm.adminAPIAddr, apiPath)

	cmdArgs := []string{
		"curl", "-s", "--show-error",
		"-X", "POST",
		"-H", "Content-Type: application/json",
		"-d", string(jsonData),
		fullAPIURL,
	}

	stdout, _, err := cm.execInCaddyContainer(cmdArgs...)
	if err != nil {
		return fmt.Errorf("failed to add route: %w", err)
	}

	// Check for Caddy errors in response
	if stdout != "" && stdout != "{}" {
		var resp map[string]any
		if json.Unmarshal([]byte(stdout), &resp) == nil {
			if _, hasError := resp["error"]; hasError {
				return fmt.Errorf("Caddy API error: %s", stdout)
			}
		}
	}

	logger.Info("Route added", "function", funcDef.Name)
	return nil
}

// RemoveFunctionRoute removes a function's route from Caddy.
func (cm *CaddyManager) RemoveFunctionRoute(funcDef FunctionDefinition) error {
	routeID := "smolfaas-func-" + funcDef.Name
	logger.Info("Removing Caddy route", "function", funcDef.Name)

	apiPath := fmt.Sprintf("/id/%s", routeID)
	fullAPIURL := fmt.Sprintf("http://%s%s", cm.adminAPIAddr, apiPath)

	cmdArgs := []string{
		"curl", "-s", "--show-error",
		"-X", "DELETE",
		fullAPIURL,
	}

	stdout, stderr, err := cm.execInCaddyContainer(cmdArgs...)
	if err != nil {
		// 404 is OK for removal
		if strings.Contains(stderr, "404") || strings.Contains(stdout, "unknown identifier") {
			return nil
		}
		return fmt.Errorf("failed to remove route: %w", err)
	}

	logger.Info("Route removed", "function", funcDef.Name)
	return nil
}

// Shutdown stops and removes the Caddy container.
func (cm *CaddyManager) Shutdown() error {
	logger.Info("Shutting down Caddy router")

	contJSON, err := cm.dockerClient.ContainerInspect(cm.ctx, cm.containerName)
	if err != nil {
		if docker.IsErrNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to inspect container: %w", err)
	}

	if contJSON.State.Running {
		timeout := 10
		cm.dockerClient.ContainerStop(cm.ctx, contJSON.ID, container.StopOptions{Timeout: &timeout})
	}

	if err := cm.dockerClient.ContainerRemove(cm.ctx, contJSON.ID, container.RemoveOptions{Force: true}); err != nil {
		return fmt.Errorf("failed to remove container: %w", err)
	}

	logger.Info("Caddy router stopped")
	return nil
}

// RemoveVolumes removes Caddy's persistent volumes.
func (cm *CaddyManager) RemoveVolumes() error {
	for _, vol := range []string{cm.caddyDataVolume, cm.caddyConfigVolume} {
		if err := cm.dockerClient.VolumeRemove(cm.ctx, vol, true); err != nil {
			if !docker.IsErrNotFound(err) {
				logger.Warn("Failed to remove volume", "volume", vol, "error", err)
			}
		}
	}
	return nil
}

// ============================================================================
// Utility Functions
// ============================================================================

// removeDockerNetwork removes a Docker network.
func removeDockerNetwork(ctx context.Context, client *docker.Client, name string) error {
	logger.Info("Removing network", "name", name)
	err := client.NetworkRemove(ctx, name)
	if err != nil {
		if docker.IsErrNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to remove network: %w", err)
	}
	return nil
}

// removeDockerVolume removes a Docker volume.
func removeDockerVolume(ctx context.Context, client *docker.Client, name string) error {
	logger.Info("Removing volume", "name", name)
	err := client.VolumeRemove(ctx, name, true)
	if err != nil {
		if docker.IsErrNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to remove volume: %w", err)
	}
	return nil
}

// pruneDanglingImages removes dangling Docker images.
func pruneDanglingImages(ctx context.Context, client *docker.Client) error {
	logger.Info("Pruning dangling images")
	_, err := client.ImagesPrune(ctx, filters.NewArgs(filters.Arg("dangling", "true")))
	return err
}

// ============================================================================
// Consistency Checker
// ============================================================================

// ConsistencyChecker verifies the consistency of SmolFaaS state.
type ConsistencyChecker struct {
	dockerClient *docker.Client
	hashMgr      *HashManager
	ctx          context.Context
	imagePrefix  string
	functionsDir string
}

// CheckResult represents the result of a consistency check for a function.
type CheckResult struct {
	FunctionName      string
	SourceHashMatch   bool
	SourceHashMessage string
	ContainerOK       bool
	ContainerMessage  string
	ImageOK           bool
	ImageMessage      string
	RouteOK           bool
	RouteMessage      string
}

// RunCheck performs a consistency check on all functions.
func (cc *ConsistencyChecker) RunCheck() ([]CheckResult, error) {
	logger.Info("Running consistency check")

	var results []CheckResult

	// Get all stored states
	states, err := cc.hashMgr.GetAllStates()
	if err != nil {
		return nil, fmt.Errorf("failed to get stored states: %w", err)
	}

	// Also discover current functions
	entries, err := os.ReadDir(cc.functionsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read functions directory: %w", err)
	}

	// Create a map of stored states by name
	stateMap := make(map[string]FunctionState)
	for _, state := range states {
		stateMap[state.Name] = state
	}

	// Check each function directory
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		funcName := entry.Name()
		funcPath := filepath.Join(cc.functionsDir, funcName)

		// Detect if this is a valid function
		var isValid bool
		for _, handler := range runtimeRegistry {
			if handler.Detect(funcPath) {
				isValid = true
				break
			}
		}

		if !isValid {
			continue
		}

		result := CheckResult{FunctionName: funcName}

		// Check source hash
		currentHash, err := cc.hashMgr.ComputeHash(funcPath)
		if err != nil {
			result.SourceHashMatch = false
			result.SourceHashMessage = fmt.Sprintf("Failed to compute hash: %v", err)
		} else if state, ok := stateMap[funcName]; ok {
			if currentHash == state.ContentHash {
				result.SourceHashMatch = true
				result.SourceHashMessage = "Source hash matches stored state"
			} else {
				result.SourceHashMatch = false
				result.SourceHashMessage = "Source hash changed (rebuild needed)"
			}
		} else {
			result.SourceHashMatch = false
			result.SourceHashMessage = "No stored state (never built)"
		}

		// Check container
		containerName := fmt.Sprintf("%s-%s", cc.imagePrefix, funcName)
		contJSON, err := cc.dockerClient.ContainerInspect(cc.ctx, containerName)
		if err != nil {
			if docker.IsErrNotFound(err) {
				result.ContainerOK = false
				result.ContainerMessage = "Container not found"
			} else {
				result.ContainerOK = false
				result.ContainerMessage = fmt.Sprintf("Error: %v", err)
			}
		} else {
			if contJSON.State.Running {
				result.ContainerOK = true
				result.ContainerMessage = "Container running"
			} else {
				result.ContainerOK = false
				result.ContainerMessage = fmt.Sprintf("Container not running (status: %s)", contJSON.State.Status)
			}
		}

		// Check image
		imageName := fmt.Sprintf("%s-%s:latest", cc.imagePrefix, funcName)
		imgInspect, err := cc.dockerClient.ImageInspect(cc.ctx, imageName)
		if err != nil {
			if docker.IsErrNotFound(err) {
				result.ImageOK = false
				result.ImageMessage = "Image not found"
			} else {
				result.ImageOK = false
				result.ImageMessage = fmt.Sprintf("Error: %v", err)
			}
		} else {
			if state, ok := stateMap[funcName]; ok && state.ImageID == imgInspect.ID {
				result.ImageOK = true
				result.ImageMessage = "Image matches stored state"
			} else if _, ok := stateMap[funcName]; !ok {
				result.ImageOK = true
				result.ImageMessage = "Image exists (no stored state)"
			} else {
				result.ImageOK = false
				result.ImageMessage = "Image ID mismatch"
			}
		}

		results = append(results, result)
		delete(stateMap, funcName)
	}

	// Report orphaned states (functions that no longer exist)
	for name := range stateMap {
		results = append(results, CheckResult{
			FunctionName:      name,
			SourceHashMatch:   false,
			SourceHashMessage: "Function directory not found (orphaned state)",
			ContainerOK:       false,
			ContainerMessage:  "N/A",
			ImageOK:           false,
			ImageMessage:      "N/A",
		})
	}

	return results, nil
}

// ============================================================================
// Cobra Command Definitions
// ============================================================================

var rootCmd = &cobra.Command{
	Use:   "smolfaas",
	Short: "A simple FaaS build and management tool",
	Long:  `SmolFaaS builds function images via BuildKit and manages a Caddy router.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		logger.Info("Initializing SmolFaaS")
		cmdCtx = context.Background()

		var err error
		cmdDockerClient, err = docker.NewClientWithOpts(docker.FromEnv, docker.WithAPIVersionNegotiation())
		if err != nil {
			return fmt.Errorf("failed to create Docker client: %w", err)
		}

		buildkitCacheVolumeName = buildkitContainerName + "-cache"

		// Ensure network exists
		_, err = cmdDockerClient.NetworkInspect(cmdCtx, networkName, network.InspectOptions{})
		if err != nil {
			if docker.IsErrNotFound(err) {
				logger.Info("Creating Docker network", "name", networkName)
				_, err = cmdDockerClient.NetworkCreate(cmdCtx, networkName, network.CreateOptions{Driver: "bridge"})
				if err != nil {
					return fmt.Errorf("failed to create network: %w", err)
				}
			} else {
				return fmt.Errorf("failed to inspect network: %w", err)
			}
		}

		return nil
	},
	PersistentPostRunE: func(cmd *cobra.Command, args []string) error {
		if cmdDockerClient != nil {
			cmdDockerClient.Close()
		}
		return nil
	},
}

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Build functions, start router, and configure routes",
	Long:  `Discovers functions, builds images via BuildKit, starts containers, and configures Caddy routing.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		logger.Info("--- Running SmolFaaS Up ---")

		// Initialize hash manager
		hashMgr, err := NewHashManager(stateDBPath)
		if err != nil {
			return fmt.Errorf("failed to init HashManager: %w", err)
		}
		defer hashMgr.Close()

		// Initialize BuildKit
		buildkitMgr, err := NewBuildkitManager(cmdCtx, cmdDockerClient, buildkitContainerName, buildkitCacheVolumeName)
		if err != nil {
			return fmt.Errorf("failed to init BuildkitManager: %w", err)
		}

		if err := buildkitMgr.ensureBuildkitRunning(); err != nil {
			return fmt.Errorf("failed to start BuildKit: %w", err)
		}
		if err := buildkitMgr.connectToBuildkit(); err != nil {
			return fmt.Errorf("failed to connect to BuildKit: %w", err)
		}

		// Build Caddy router image first
		caddyMgr, err := NewCaddyManager(cmdCtx, cmdDockerClient, caddyContainerName, networkName)
		if err != nil {
			return fmt.Errorf("failed to init CaddyManager: %w", err)
		}

		logger.Info("Building Caddy router image")
		if err := caddyMgr.BuildRouterImage(buildkitMgr, functionsAddr); err != nil {
			return fmt.Errorf("failed to build Caddy image: %w", err)
		}

		// Build functions
		faasBuilder := NewFaaSBuilder(buildkitMgr, hashMgr, faasImagePrefix)
		readyFunctions, buildErr := faasBuilder.BuildAndCheckAllFunctions(functionsDir)

		// Shutdown BuildKit
		logger.Info("Cleaning up BuildKit")
		buildkitMgr.shutdown()
		buildkitMgr.StopAndRemoveDaemon()

		if buildErr != nil {
			logger.Warn("Build errors occurred", "error", buildErr)
			if len(readyFunctions) == 0 {
				return fmt.Errorf("no functions ready: %w", buildErr)
			}
		}

		if len(readyFunctions) == 0 {
			logger.Info("No functions to start")
			return nil
		}

		// Start function containers
		funcMgr, err := NewFunctionManager(cmdCtx, cmdDockerClient, networkName)
		if err != nil {
			return fmt.Errorf("failed to init FunctionManager: %w", err)
		}
		if err := funcMgr.StartAll(readyFunctions); err != nil {
			return fmt.Errorf("failed to start functions: %w", err)
		}

		// Start Caddy router
		if err := caddyMgr.Start(); err != nil {
			return fmt.Errorf("failed to start Caddy: %w", err)
		}

		// Configure routes
		logger.Info("Configuring Caddy routes")
		for _, fn := range readyFunctions {
			if err := caddyMgr.AddFunctionRoute(fn); err != nil {
				logger.Warn("Failed to add route", "function", fn.Name, "error", err)
			}
		}

		logger.Info("--- SmolFaaS Up Complete ---", "functions", len(readyFunctions))
		return nil
	},
}

var downCmd = &cobra.Command{
	Use:   "down [function-name...]",
	Short: "Stop and remove router and function containers",
	Long: `Stops and removes SmolFaaS services.
Without arguments: stops router and all function containers.
With function names: stops only specified functions and removes their images/routes.
Use --force for full cleanup including volumes, network, and dangling images.`,
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		logger.Info("--- Running SmolFaaS Down ---")

		hashMgr, err := NewHashManager(stateDBPath)
		if err != nil {
			logger.Warn("Failed to init HashManager", "error", err)
		}
		defer func() {
			if hashMgr != nil {
				hashMgr.Close()
			}
		}()

		funcMgr, err := NewFunctionManager(cmdCtx, cmdDockerClient, networkName)
		if err != nil {
			logger.Warn("Failed to init FunctionManager", "error", err)
		}

		caddyMgr, err := NewCaddyManager(cmdCtx, cmdDockerClient, caddyContainerName, networkName)
		if err != nil {
			logger.Warn("Failed to init CaddyManager", "error", err)
		}

		if len(args) == 0 {
			// Full teardown
			logger.Info("Performing full teardown")

			if funcMgr != nil {
				funcMgr.StopAllByPrefix(faasImagePrefix)
			}
			if caddyMgr != nil {
				caddyMgr.Shutdown()
			}

			if forceCleanup {
				logger.Info("Force cleanup: removing volumes, network, and images")
				if caddyMgr != nil {
					caddyMgr.RemoveVolumes()
				}
				removeDockerVolume(cmdCtx, cmdDockerClient, buildkitCacheVolumeName)
				removeDockerNetwork(cmdCtx, cmdDockerClient, networkName)
				pruneDanglingImages(cmdCtx, cmdDockerClient)

				// Remove state database
				if stateDBPath != "" {
					os.Remove(stateDBPath)
				}
			}
		} else {
			// Selective teardown
			logger.Info("Performing selective teardown", "functions", args)

			if forceCleanup {
				logger.Warn("--force flag ignored for selective teardown")
			}

			for _, funcName := range args {
				logger.Info("Tearing down function", "name", funcName)
				containerName := fmt.Sprintf("%s-%s", faasImagePrefix, funcName)
				imageName := fmt.Sprintf("%s-%s:latest", faasImagePrefix, funcName)

				if funcMgr != nil {
					funcMgr.StopAndRemove(containerName)
					funcMgr.RemoveImage(imageName)
				}

				if caddyMgr != nil {
					caddyMgr.RemoveFunctionRoute(FunctionDefinition{Name: funcName})
				}

				if hashMgr != nil {
					hashMgr.DeleteState(funcName)
				}
			}
		}

		logger.Info("--- SmolFaaS Down Complete ---")
		return nil
	},
}

var checkCmd = &cobra.Command{
	Use:   "check",
	Short: "Verify consistency of functions, containers, and routes",
	Long:  `Performs a read-only consistency check and reports any issues found.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		logger.Info("--- Running SmolFaaS Check ---")

		hashMgr, err := NewHashManager(stateDBPath)
		if err != nil {
			return fmt.Errorf("failed to init HashManager: %w", err)
		}
		defer hashMgr.Close()

		checker := &ConsistencyChecker{
			dockerClient: cmdDockerClient,
			hashMgr:      hashMgr,
			ctx:          cmdCtx,
			imagePrefix:  faasImagePrefix,
			functionsDir: functionsDir,
		}

		results, err := checker.RunCheck()
		if err != nil {
			return fmt.Errorf("check failed: %w", err)
		}

		// Display results
		fmt.Println("\nConsistency Check Results:")
		fmt.Println("==========================")

		needsAttention := 0
		for _, r := range results {
			fmt.Printf("\n%s:\n", r.FunctionName)

			if r.SourceHashMatch {
				fmt.Printf("  ✓ %s\n", r.SourceHashMessage)
			} else {
				fmt.Printf("  ✗ %s\n", r.SourceHashMessage)
				needsAttention++
			}

			if r.ContainerOK {
				fmt.Printf("  ✓ %s\n", r.ContainerMessage)
			} else {
				fmt.Printf("  ✗ %s\n", r.ContainerMessage)
			}

			if r.ImageOK {
				fmt.Printf("  ✓ %s\n", r.ImageMessage)
			} else {
				fmt.Printf("  ✗ %s\n", r.ImageMessage)
			}
		}

		fmt.Println()
		if needsAttention > 0 {
			fmt.Printf("Summary: %d function(s) need attention. Run 'smolfaas up' to fix.\n", needsAttention)
		} else {
			fmt.Println("Summary: All functions are consistent.")
		}

		logger.Info("--- SmolFaaS Check Complete ---")
		return nil
	},
}

// Execute runs the root command.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func init() {
	// Initialize logger
	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Register runtime handlers in priority order
	// Dockerfile first (highest priority), then language-specific handlers
	registerRuntime(&DockerfileRuntimeHandler{})
	registerRuntime(&GoRuntimeHandler{})
	registerRuntime(&NodeRuntimeHandler{})
	registerRuntime(&PythonRuntimeHandler{})

	// Persistent flags
	rootCmd.PersistentFlags().StringVarP(&functionsDir, "functions-dir", "d", "./functions", "Directory containing function sources")
	rootCmd.PersistentFlags().StringVarP(&functionsAddr, "addr", "a", "fn.localhost", "Router hostname")
	rootCmd.PersistentFlags().StringVarP(&networkName, "network", "n", "smolfaas", "Docker network name")
	rootCmd.PersistentFlags().StringVar(&caddyContainerName, "caddy-name", "smolfaas-router", "Caddy container name")
	rootCmd.PersistentFlags().StringVar(&buildkitContainerName, "buildkit-name", "smolfaas-buildkitd", "BuildKit container name")
	rootCmd.PersistentFlags().StringVarP(&faasImagePrefix, "image-prefix", "p", "smolfaas-func", "Image/container name prefix")
	rootCmd.PersistentFlags().StringVar(&stateDBPath, "state-db", ".smolfaas/state.db", "Path to SQLite state database")

	// Add subcommands
	rootCmd.AddCommand(upCmd)
	rootCmd.AddCommand(downCmd)
	rootCmd.AddCommand(checkCmd)

	// Down command flags
	downCmd.Flags().BoolVarP(&forceCleanup, "force", "f", false, "Remove volumes, network, and dangling images")
}

func main() {
	Execute()
}

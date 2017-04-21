// handler package implements a library for handling run lambda requests from
// the worker server.
package handler

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-lambda/open-lambda/worker/config"
	"github.com/open-lambda/open-lambda/worker/handler/state"
	"github.com/open-lambda/open-lambda/worker/pool-manager/policy"
	"github.com/open-lambda/open-lambda/worker/registry"

	pmanager "github.com/open-lambda/open-lambda/worker/pool-manager"
	sb "github.com/open-lambda/open-lambda/worker/sandbox"
)

// HandlerSet represents a collection of Handlers of a worker server. It
// manages the Handler by HandlerLRU.
type HandlerSet struct {
	mutex     sync.Mutex
	handlers  map[string]*Handler
	regMgr    registry.RegistryManager
	sbFactory sb.SandboxFactory
	poolMgr   pmanager.PoolManager
	config    *config.Config
	lru       *HandlerLRU
	workerDir string
	pipMirror string
	hhits     *int64
	ihits     *int64
	misses    *int64
}

// Handler handles requests to run a lambda on a worker server. It handles
// concurrency and communicates with the sandbox manager to change the
// state of the container that servers the lambda.
type Handler struct {
	name       string
	mutex      sync.Mutex
	hset       *HandlerSet
	sandbox    sb.Sandbox
	lastPull   *time.Time
	state      state.HandlerState
	runners    int
	code       []byte
	codeDir    string
	pkgs       []string
	sandboxDir string
	fs         *policy.ForkServer
	usage      int
}

// NewHandlerSet creates an empty HandlerSet
func NewHandlerSet(opts *config.Config) (handlerSet *HandlerSet, err error) {
	rm, err := registry.InitRegistryManager(opts)
	if err != nil {
		return nil, err
	}

	sf, err := sb.InitSandboxFactory(opts)
	if err != nil {
		return nil, err
	}

	pm, err := pmanager.InitPoolManager(opts)
	if err != nil {
		return nil, err
	}

	var hhits int64 = 0
	var ihits int64 = 0
	var misses int64 = 0
	handlers := make(map[string]*Handler)
	handlerSet = &HandlerSet{
		handlers:  handlers,
		regMgr:    rm,
		sbFactory: sf,
		poolMgr:   pm,
		workerDir: opts.Worker_dir,
		pipMirror: opts.Pip_mirror,
		hhits:     &hhits,
		ihits:     &ihits,
		misses:    &misses,
	}

	handlerSet.lru = NewHandlerLRU(handlerSet, opts.Handler_cache_size) //kb

	if pm != nil {
		go handlerSet.killOrphans()
	}

	return handlerSet, nil
}

// Get always returns a Handler, creating one if necessarily.
func (h *HandlerSet) Get(name string) *Handler {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	handler := h.handlers[name]
	if handler == nil {
		sandboxDir := path.Join(h.workerDir, "handlers", name, "sandbox")
		handler = &Handler{
			name:       name,
			hset:       h,
			state:      state.Unitialized,
			runners:    0,
			pkgs:       []string{},
			sandboxDir: sandboxDir,
		}
		h.handlers[name] = handler
	}

	return handler
}

func (h *HandlerSet) killOrphans() {
	for {
		time.Sleep(5 * time.Millisecond)
		h.mutex.Lock()
		defer h.mutex.Unlock()

		for _, handler := range h.handlers {
			handler.mutex.Lock()
			if handler.sandbox != nil && handler.fs == nil {
				h.mutex.Lock()
				h.handlers[handler.name] = nil
				h.mutex.Unlock()

				for handler.runners > 0 {
					handler.mutex.Unlock()
					time.Sleep(1 * time.Millisecond)
					handler.mutex.Lock()
				}
				go handler.nuke()
			}
			handler.mutex.Unlock()
		}

	}
}

// Dump prints the name and state of the Handlers currently in the HandlerSet.
func (h *HandlerSet) Dump() {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	log.Printf("HANDLERS:\n")
	for k, v := range h.handlers {
		log.Printf("> %v: %v\n", k, v.state.String())
	}
}

// RunStart runs the lambda handled by this Handler. It checks if the code has
// been pulled, sandbox been created, and sandbox been started. The channel of
// the sandbox of this lambda is returned.
func (h *Handler) RunStart() (ch *sb.SandboxChannel, err error) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	// get code if needed
	if h.lastPull == nil {
		codeDir, pkgs, err := h.hset.regMgr.Pull(h.name)
		if err != nil {
			return nil, err
		}

		now := time.Now()
		h.lastPull = &now
		h.codeDir = codeDir
		h.pkgs = pkgs
	}

	// create sandbox if needed
	if h.sandbox == nil {
		if err := os.MkdirAll(h.sandboxDir, 0666); err != nil {
			return nil, err
		}

		sandbox, err := h.hset.sbFactory.Create(h.codeDir, h.sandboxDir, h.hset.pipMirror)
		if err != nil {
			return nil, err
		}

		h.sandbox = sandbox
		if h.state, err = sandbox.State(); err != nil {
			return nil, err
		}

		// newly created sandbox could be in any state; let it run
		if h.state == state.Stopped {
			if err := sandbox.Start(); err != nil {
				return nil, err
			}
		} else if h.state == state.Paused {
			if err := sandbox.Unpause(); err != nil {
				return nil, err
			}
		}

		hit := false
		if h.hset.poolMgr != nil {
			containerSB, ok := h.sandbox.(sb.ContainerSandbox)
			if !ok {
				return nil, errors.New("forkenter only supported with ContainerSandbox")
			}

			if h.fs, hit, err = h.hset.poolMgr.Provision(containerSB, h.sandboxDir, h.pkgs); err != nil {
				return nil, err
			}
		}

		if hit {
			atomic.AddInt64(h.hset.ihits, 1)
		} else {
			atomic.AddInt64(h.hset.misses, 1)
		}

		sockPath := fmt.Sprintf("%s/ol.sock", h.sandboxDir)

		// wait up to 30s for server to initialize
		start := time.Now()
		for ok := true; ok; ok = os.IsNotExist(err) {
			_, err = os.Stat(sockPath)
			if time.Since(start).Seconds() > 45 {
				return nil, errors.New(fmt.Sprintf("handler server failed to initialize after 30s"))
			}
		}

	} else if h.state == state.Paused { // unpause if paused
		atomic.AddInt64(h.hset.hhits, 1)
		if err := h.sandbox.Unpause(); err != nil {
			return nil, err
		}
		h.hset.lru.Remove(h)
	} else {
		atomic.AddInt64(h.hset.hhits, 1)
	}

	h.state = state.Running
	h.runners += 1

	log.Printf("handler hits: %v, import hits: %v, misses: %v", *h.hset.hhits, *h.hset.ihits, *h.hset.misses)
	return h.sandbox.Channel()
}

// RunFinish notifies that a request to run the lambda has completed. If no
// request is being run in its sandbox, sandbox will be paused and the handler
// be added to the HandlerLRU.
func (h *Handler) RunFinish() {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	h.runners -= 1

	// are we the last?
	if h.runners == 0 {
		if err := h.sandbox.Pause(); err != nil {
			// TODO(tyler): better way to handle this?  If
			// we can't pause, the handler gets to keep
			// running for free...
			log.Printf("Could not pause %v!  Error: %v\n", h.name, err)
		}
		h.state = state.Paused
		h.hset.lru.Add(h)
	}
}

// StopIfPaused stops the sandbox if it is paused.
func (h *Handler) nuke() {
	h.sandbox.Unpause()
	h.sandbox.Stop()
	h.sandbox.Remove()
}

// Sandbox returns the sandbox of this Handler.
func (h *Handler) Sandbox() sb.Sandbox {
	return h.sandbox
}

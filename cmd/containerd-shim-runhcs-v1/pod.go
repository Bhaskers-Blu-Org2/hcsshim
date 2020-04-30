package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Microsoft/hcsshim/internal/hcsoci"
	"github.com/Microsoft/hcsshim/internal/log"
	"github.com/Microsoft/hcsshim/internal/oci"
	"github.com/Microsoft/hcsshim/internal/uvm"
	"github.com/Microsoft/hcsshim/osversion"
	eventstypes "github.com/containerd/containerd/api/events"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/runtime"
	"github.com/containerd/containerd/runtime/v2/task"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
)

// shimPod represents the logical grouping of all tasks in a single set of
// shared namespaces. The pod sandbox (container) is represented by the task
// that matches the `shimPod.ID()`
type shimPod interface {
	// ID is the id of the task representing the pause (sandbox) container.
	ID() string
	// CreateTask creates a workload task within this pod named `tid` with
	// settings `s`.
	//
	// If `tid==ID()` or `tid` is the same as any other task in this pod, this
	// pod MUST return `errdefs.ErrAlreadyExists`.
	CreateTask(ctx context.Context, req *task.CreateTaskRequest, s *specs.Spec) (shimTask, error)
	// GetTask returns a task in this pod that matches `tid`.
	//
	// If `tid` is not found, this pod MUST return `errdefs.ErrNotFound`.
	GetTask(tid string) (shimTask, error)
	// KillTask sends `signal` to task that matches `tid`.
	//
	// If `tid` is not found, this pod MUST return `errdefs.ErrNotFound`.
	//
	// If `tid==ID() && eid == "" && all == true` this pod will send `signal` to
	// all tasks in the pod and lastly send `signal` to the sandbox itself.
	//
	// If `all == true && eid != ""` this pod MUST return
	// `errdefs.ErrFailedPrecondition`.
	//
	// A call to `KillTask` is only valid when the exec found by `tid,eid` is in
	// the `shimExecStateRunning, shimExecStateExited` states. If the exec is
	// not in this state this pod MUST return `errdefs.ErrFailedPrecondition`.
	KillTask(ctx context.Context, tid, eid string, signal uint32, all bool) error
}

func createPod(ctx context.Context, events publisher, req *task.CreateTaskRequest, s *specs.Spec) (shimPod, error) {
	log.G(ctx).WithField("tid", req.ID).Debug("createPod")

	if osversion.Get().Build < osversion.RS5 {
		return nil, errors.Wrapf(errdefs.ErrFailedPrecondition, "pod support is not available on Windows versions previous to RS5 (%d)", osversion.RS5)
	}

	ct, sid, err := oci.GetSandboxTypeAndID(s.Annotations)
	if err != nil {
		return nil, err
	}
	if ct != oci.KubernetesContainerTypeSandbox {
		return nil, errors.Wrapf(
			errdefs.ErrFailedPrecondition,
			"expected annotation: '%s': '%s' got '%s'",
			oci.KubernetesContainerTypeAnnotation,
			oci.KubernetesContainerTypeSandbox,
			ct)
	}
	if sid != req.ID {
		return nil, errors.Wrapf(
			errdefs.ErrFailedPrecondition,
			"expected annotation '%s': '%s' got '%s'",
			oci.KubernetesSandboxIDAnnotation,
			req.ID,
			sid)
	}

	owner := filepath.Base(os.Args[0])
	isWCOW := oci.IsWCOW(s)

	templateID := oci.ParseAnnotationsTemplateID(ctx, s)
	var utc *uvm.UVMTemplateConfig
	if templateID != "" && isWCOW {
		utc, err = FetchTemplateConfig(ctx, templateID)
		if err != nil {
			return nil, err
		}
	}

	var parent *uvm.UtilityVM
	if oci.IsIsolated(s) {
		// Create the UVM parent
		opts, err := oci.SpecToUVMCreateOpts(ctx, s, fmt.Sprintf("%s@vm", req.ID), owner)
		if err != nil {
			return nil, err
		}
		switch opts.(type) {
		case *uvm.OptionsLCOW:
			lopts := (opts).(*uvm.OptionsLCOW)
			parent, err = uvm.CreateLCOW(ctx, lopts)
			if err != nil {
				return nil, err
			}
		case *uvm.OptionsWCOW:
			wopts := (opts).(*uvm.OptionsWCOW)

			// In order for the UVM sandbox.vhdx not to collide with the actual
			// nested Argon sandbox.vhdx we append the \vm folder to the last
			// entry in the list.
			layersLen := len(s.Windows.LayerFolders)
			layers := make([]string, layersLen)
			copy(layers, s.Windows.LayerFolders)

			vmPath := filepath.Join(layers[layersLen-1], "vm")
			err := os.MkdirAll(vmPath, 0)
			if err != nil {
				return nil, err
			}
			layers[layersLen-1] = vmPath
			wopts.LayerFolders = layers
			if utc != nil {
				parent, err = uvm.CloneWCOW(ctx, wopts, utc)
			} else {
				parent, err = uvm.CreateWCOW(ctx, wopts)
			}
			if err != nil {
				return nil, err
			}
		}
		if utc != nil {
			err = parent.StartClone(ctx)
		} else {
			err = parent.Start(ctx)
		}
		if err != nil {
			parent.Close()
			return nil, fmt.Errorf("Error starting UVM: %s", err)
		}
	} else if !isWCOW {
		return nil, errors.Wrap(errdefs.ErrFailedPrecondition, "oci spec does not contain WCOW or LCOW spec")
	}
	defer func() {
		// clean up the uvm if we fail any further operations
		if err != nil && parent != nil {
			parent.Close()
		}
	}()

	p := pod{
		events: events,
		id:     req.ID,
		host:   parent,
	}

	// TOOD: JTERRY75 - There is a bug in the compartment activation for Windows
	// Process isolated that requires us to create the real pause container to
	// hold the network compartment open. This is not required for Windows
	// Hypervisor isolated. When we have a build that supports this for Windows
	// Process isolated make sure to move back to this model.
	if isWCOW && parent != nil {

		if s.Windows != nil && s.Windows.Network != nil {
			if utc != nil {
				if len(utc.NetNSIDs) > 0 {
					err = hcsoci.CloneEndpoints(ctx, parent, s.Windows.Network.NetworkNamespace, utc.NetNSIDs[0])
				} else {
					log.G(ctx).Warnf("No network namespace provided in template %s", utc.UVMID)
				}
			} else {
				err = hcsoci.SetupNetworkNamespace(ctx, parent, s.Windows.Network.NetworkNamespace)
			}
			if err != nil {
				return nil, fmt.Errorf("failed to setup network namespace for pod: %s", err)
			}
		}

		// For WCOW we fake out the init task since we dont need it. We only
		// need to provision the guest network namespace if this is hypervisor
		// isolated. Process isolated WCOW gets the namespace endpoints
		// automatically.
		p.sandboxTask = newWcowPodSandboxTask(ctx, events, req.ID, req.Bundle, parent)
		// Publish the created event. We only do this for a fake WCOW task. A
		// HCS Task will event itself based on actual process lifetime.
		events.publishEvent(
			ctx,
			runtime.TaskCreateEventTopic,
			&eventstypes.TaskCreate{
				ContainerID: req.ID,
				Bundle:      req.Bundle,
				Rootfs:      req.Rootfs,
				IO: &eventstypes.TaskIO{
					Stdin:    req.Stdin,
					Stdout:   req.Stdout,
					Stderr:   req.Stderr,
					Terminal: req.Terminal,
				},
				Checkpoint: "",
				Pid:        0,
			})
	} else {
		if isWCOW {
			// The pause container activation will immediately exit on Windows
			// because there is no command. We forcibly update the command here
			// to keep it alive.
			s.Process.CommandLine = "cmd /c ping -t 127.0.0.1 > nul"
		}
		// LCOW (and WCOW Process Isolated for the time being) requires a real
		// task for the sandbox.
		lt, err := newHcsTask(ctx, events, parent, true, req, s)
		if err != nil {
			return nil, err
		}
		p.sandboxTask = lt
	}
	return &p, nil
}

var _ = (shimPod)(&pod{})

type pod struct {
	events publisher
	// id is the id of the sandbox task when the pod is created.
	//
	// It MUST be treated as read only in the lifetime of the pod.
	id string
	// sandboxTask is the task that represents the sandbox.
	//
	// Note: The invariant `id==sandboxTask.ID()` MUST be true.
	//
	// It MUST be treated as read only in the lifetime of the pod.
	sandboxTask shimTask
	// host is the UtilityVM that is hosting `sandboxTask` if the task is
	// hypervisor isolated.
	//
	// It MUST be treated as read only in the lifetime of the pod.
	host *uvm.UtilityVM

	// wcl is the worload create mutex. All calls to CreateTask must hold this
	// lock while the ID reservation takes place. Once the ID is held it is safe
	// to release the lock to allow concurrent creates.
	wcl           sync.Mutex
	workloadTasks sync.Map
}

func (p *pod) ID() string {
	return p.id
}

func (p *pod) CreateTask(ctx context.Context, req *task.CreateTaskRequest, s *specs.Spec) (_ shimTask, err error) {
	if req.ID == p.id {
		return nil, errors.Wrapf(errdefs.ErrAlreadyExists, "task with id: '%s' already exists", req.ID)
	}
	e, _ := p.sandboxTask.GetExec("")
	if e.State() != shimExecStateRunning {
		return nil, errors.Wrapf(errdefs.ErrFailedPrecondition, "task with id: '%s' cannot be created in pod: '%s' which is not running", req.ID, p.id)
	}

	p.wcl.Lock()
	_, loaded := p.workloadTasks.LoadOrStore(req.ID, nil)
	if loaded {
		return nil, errors.Wrapf(errdefs.ErrAlreadyExists, "task with id: '%s' already exists id pod: '%s'", req.ID, p.id)
	}
	p.wcl.Unlock()
	defer func() {
		if err != nil {
			p.workloadTasks.Delete(req.ID)
		}
	}()

	templateID := oci.ParseAnnotationsTemplateID(ctx, s)
	saveAsTemplate := oci.ParseAnnotationsSaveAsTemplate(ctx, s)
	if templateID != "" && saveAsTemplate {
		return nil, errors.Wrapf(errdefs.ErrInvalidArgument, "templateID and save as template flags can not be passed in the same request.")
	}

	if (saveAsTemplate || templateID != "") && (p.host == nil || p.host.OS() != "windows") {
		return nil, errors.Wrapf(errdefs.ErrInvalidArgument, "Save as template and creating clones is only available for WCOW.")
	}

	ct, sid, err := oci.GetSandboxTypeAndID(s.Annotations)
	if err != nil {
		return nil, err
	}
	if ct != oci.KubernetesContainerTypeContainer {
		return nil, errors.Wrapf(
			errdefs.ErrFailedPrecondition,
			"expected annotation: '%s': '%s' got '%s'",
			oci.KubernetesContainerTypeAnnotation,
			oci.KubernetesContainerTypeContainer,
			ct)
	}
	if sid != p.id {
		return nil, errors.Wrapf(
			errdefs.ErrFailedPrecondition,
			"expected annotation '%s': '%s' got '%s'",
			oci.KubernetesSandboxIDAnnotation,
			p.id,
			sid)
	}

	var st shimTask
	if templateID != "" {
		st, err = newClonedHcsTask(ctx, p.events, p.host, false, req, s, templateID)
	} else {
		st, err = newHcsTask(ctx, p.events, p.host, false, req, s)
	}
	if err != nil {
		return nil, err
	}

	p.workloadTasks.Store(req.ID, st)
	return st, nil
}

func (p *pod) GetTask(tid string) (shimTask, error) {
	if tid == p.id {
		return p.sandboxTask, nil
	}
	raw, loaded := p.workloadTasks.Load(tid)
	if !loaded {
		return nil, errors.Wrapf(errdefs.ErrNotFound, "task with id: '%s' not found", tid)
	}
	return raw.(shimTask), nil
}

func (p *pod) KillTask(ctx context.Context, tid, eid string, signal uint32, all bool) error {
	t, err := p.GetTask(tid)
	if err != nil {
		return err
	}
	if all && eid != "" {
		return errors.Wrapf(errdefs.ErrFailedPrecondition, "cannot signal all with non empty ExecID: '%s'", eid)
	}
	eg := errgroup.Group{}
	if all && tid == p.id {
		// We are in a kill all on the sandbox task. Signal everything.
		p.workloadTasks.Range(func(key, value interface{}) bool {
			wt := value.(shimTask)
			eg.Go(func() error {
				return wt.KillExec(ctx, eid, signal, all)
			})

			// iterate all
			// TODO(ambarve): shouldn't this be true to continue iteration?
			return false
		})
	}
	eg.Go(func() error {
		return t.KillExec(ctx, eid, signal, all)
	})
	return eg.Wait()
}

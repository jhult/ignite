package gitops

import (
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/weaveworks/ignite/pkg/apis/ignite/validation"
	"github.com/weaveworks/ignite/pkg/dmlegacy"
	"github.com/weaveworks/ignite/pkg/operations"
	"github.com/weaveworks/ignite/pkg/storage/cache"
	"github.com/weaveworks/ignite/pkg/storage/manifest"
	"github.com/weaveworks/ignite/pkg/storage/watch/update"
	"github.com/weaveworks/ignite/pkg/util"
	api "github.com/weaveworks/ignite/pkg/apis/ignite"
	meta "github.com/weaveworks/ignite/pkg/apis/meta/v1alpha1"
	"github.com/weaveworks/ignite/pkg/client"
	"github.com/weaveworks/ignite/pkg/gitops/gitdir"
)

var (
	vmMap           map[meta.UID]*api.VM
	s *manifest.ManifestStorage
	c               *client.Client
	gitDir          *gitdir.GitDirectory
	syncInterval, _ = time.ParseDuration("10s")
)

func RunLoop(url, branch string, paths []string) error {
	log.Printf("Starting GitOps loop for repo at %q\n", url)
	log.Printf("Whenever changes are pushed to the %s branch, Ignite will apply the desired state locally\n", branch)
	log.Println("Initializing the Git repo...")

	// Construct the GitDirectory implementation which backs the storage
	gitDir = gitdir.NewGitDirectory(url, branch, paths, syncInterval)
	// Start the GitDirectory sync loop
	gitDir.StartLoop()
	
	for {
		// Wait for changes to happen in the Git repo
		log.Println("Waiting for updates in the Git repo...")
		_ = gitDir.WaitForUpdate()

		// The first time this is run, when the initial clone is done, we'll populate the storage and the client
		var err error
		if s == nil {
			// Construct a manifest storage for the path backed by git
			s, err = manifest.NewManifestStorage(gitDir.Dir())
			if err != nil {
				return err
			}
			// Wrap the Manifest Storage with a cache for better performance, and create a client
			c = client.NewClient(cache.NewCache(s))
		}

		time.Sleep(5 * time.Second)

		// When we know the underlying state has changed, reload the storage mappings, and get what's changed
		diff := s.Sync()
		log.Debugf("diff %v", diff)

		list, err := c.VMs().List()
		if err != nil {
			log.Warnf("Listing VMs returned an error: %v. Retrying...", err)
			continue
		}

		vmMap = mapVMs(list)

		wg := &sync.WaitGroup{}
		for _, file := range diff {
			vm := vmMap[file.APIType.GetUID()]
			if vm == nil {
				if file.Event != update.EventDelete {
					// This is unexpected
					log.Warn("Skipping %s of %s with UID %s, no such object found through the client.", file.Event, file.APIType.GetKind(), file.APIType.GetUID())
					continue
				}

				// As we know this VM was deleted, it's logical that it wasn't found in the VMs().List() call above
				// Construct a temporary VM object for passing to the delete function
				vm = &api.VM{
					TypeMeta:   *file.APIType.GetTypeMeta(),
					ObjectMeta: *file.APIType.GetObjectMeta(),
					Status: api.VMStatus{
						State: api.VMStateStopped,
					},
				}
			} else {
				// If the object was existent in the storage; validate it
				// Validate the VM object
				if err := validation.ValidateVM(vm).ToAggregate(); err != nil {
					log.Warn("Skipping %s of %s with UID %s, VM not valid %v.", file.Event, file.APIType.GetKind(), file.APIType.GetUID(), err)
					continue
				}
			}

			// Construct the runtime object for this VM. This will also do defaulting
			// TODO: Consider cleanup like this?
			//defer metadata.Cleanup(vm, false) // TODO: Handle silent
			//return metadata.Success(vm)

			// TODO: At the moment there aren't running in parallel, shall they?
			switch file.Event {
			case update.EventCreate:
				// TODO: Run this as a goroutine
				runHandle(wg, func() error {
					return handleCreate(vm)
				})
			case update.EventModify:
				// TODO: Run this as a goroutine
				runHandle(wg, func() error {
					return handleChange(vm)
				})
			case update.EventDelete:
				// TODO: Run this as a goroutine
				runHandle(wg, func() error {
					// TODO: Temporary VM Object for removal
					return handleDelete(vm)
				})
			default:
				log.Printf("Unrecognized Git update type %s\n", file.Event)
				continue
			}
		}

		// wait for all goroutines to finish before the next sync round
		wg.Wait()
	}
}

func runHandle(wg *sync.WaitGroup, fn func() error) {
	wg.Add(1)
	defer wg.Done()

	if err := fn(); err != nil {
		log.Errorf("An error occurred when processing a VM update: %v\n", err)
	}
}

func mapVMs(vmlist []*api.VM) map[meta.UID]*api.VM {
	result := map[meta.UID]*api.VM{}
	for _, vm := range vmlist {
		result[vm.UID] = vm
	}

	return result
}

func handleCreate(vm *api.VM) error {
	var err error

	switch vm.Status.State {
	case api.VMStateCreated:
		err = create(vm)
	case api.VMStateRunning:
		err = start(vm)
	case api.VMStateStopped:
		log.Printf("VM %q was added to git with status Stopped, nothing to do\n", vm.GetUID())
	default:
		log.Printf("Unknown state of VM %q: %s", vm.GetUID().String(), vm.Status.State)
	}

	return err
}

func handleChange(vm *api.VM) error {
	var err error

	switch vm.Status.State {
	case api.VMStateCreated:
		err = fmt.Errorf("VM %q cannot changed into the Created state", vm.GetUID())
	case api.VMStateRunning:
		err = start(vm)
	case api.VMStateStopped:
		err = stop(vm)
	default:
		log.Printf("Unknown state of VM %q: %s", vm.GetUID().String(), vm.Status.State)
	}

	return err
}

func handleDelete(vm *api.VM) error {
	return remove(vm)
}

// TODO: Unify this with the "real" Create() method currently in cmd/
func create(vm *api.VM) error {
	log.Printf("Creating VM %q with name %q...", vm.GetUID(), vm.GetName())
	if err := ensureOCIImages(vm); err != nil {
		return err
	}

	// Allocate and populate the overlay file
	return dmlegacy.AllocateAndPopulateOverlay(vm)
}

// ensureOCIImages imports the base/kernel OCI images if needed
func ensureOCIImages(vm *api.VM) error {
	// Check if a image with this name already exists, or import it
	image, err := operations.FindOrImportImage(c, vm.Spec.Image.OCIClaim.Ref)
	if err != nil {
		return err
	}

	// Populate relevant data from the Image on the VM object
	vm.SetImage(image)

	// Check if a kernel with this name already exists, or import it
	kernel, err := operations.FindOrImportKernel(c, vm.Spec.Kernel.OCIClaim.Ref)
	if err != nil {
		return err
	}

	// Populate relevant data from the Kernel on the VM object
	vm.SetKernel(kernel)

	// Save the file to disk. This will also write the file to /var/lib/firecracker for compability
	return c.VMs().Set(vm)
}

func start(vm *api.VM) error {
	// create the overlay if it doesn't exist
	if !util.FileExists(vm.OverlayFile()) {
		if err := create(vm); err != nil {
			return err
		}
	}

	log.Printf("Starting VM %q with name %q...", vm.GetUID(), vm.GetName())
	return operations.StartVM(vm, true)
}

func stop(vm *api.VM) error {
	log.Printf("Stopping VM %q with name %q...", vm.GetUID(), vm.GetName())
	return operations.StopVM(vm, true, false)
}

func remove(vm *api.VM) error {
	log.Printf("Removing VM %q with name %q...", vm.GetUID(), vm.GetName())
	return operations.RemoveVM(c, vm)
}

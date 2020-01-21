package machines

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/k0kubun/pp"
	"github.com/ubuntu/zsys/internal/config"
	"github.com/ubuntu/zsys/internal/i18n"
	"github.com/ubuntu/zsys/internal/log"
	"github.com/ubuntu/zsys/internal/zfs"
)

// Machines hold a zfs system states, with a map of main root system dataset name to a given Machine,
// current machine and nextState if an upgrade has been proceeded.
type Machines struct {
	all               map[string]*Machine
	cmdline           string
	current           *Machine
	nextState         *State
	allSystemDatasets []*zfs.Dataset
	allUsersDatasets  []*zfs.Dataset

	z *zfs.Zfs
}

// Machine is a group of Main and its History children states
type Machine struct {
	// Main machine State
	State
	// Users is a per user reference to each of its state
	Users map[string]map[string]UserState `json:",omitempty"`
	// History is a map or root system datasets to all its possible State
	History map[string]*State `json:",omitempty"`
}

// State is a finite regroupement of multiple ID and elements corresponding to a bootable machine instance.
type State struct {
	// ID is the path to the root system dataset for this State.
	ID string
	// IsZsys states if we have a zsys system. The other datasets type will be empty otherwise.
	IsZsys bool `json:",omitempty"`
	// LastUsed is the last time this state was used
	LastUsed *time.Time `json:",omitempty"`
	// SystemDatasets are all datasets that constitutes this State (in <pool>/ROOT/ + <pool>/BOOT/)
	SystemDatasets []*zfs.Dataset `json:",omitempty"`
	// UserDatasets are all datasets that are attached to the given State (in <pool>/USERDATA/)
	UserDatasets []*zfs.Dataset `json:",omitempty"`
	// PersistentDatasets are all datasets that are canmount=on and and not in ROOT, USERDATA or BOOT dataset containers.
	// Those are common between all machines, as persistent (and detected without snapshot information)
	PersistentDatasets []*zfs.Dataset `json:",omitempty"`
}

// UserState maps a particular state to all the datasets owned by a user
type UserState struct {
	ID       string
	Datasets []*zfs.Dataset
}

const (
	userdatasetsContainerName = "/userdata/"
)

// WithLibZFS allows overriding default libzfs implementations with a mock
func WithLibZFS(libzfs zfs.LibZFSInterface) func(o *options) error {
	return func(o *options) error {
		o.libzfs = libzfs
		return nil
	}
}

type options struct {
	libzfs zfs.LibZFSInterface
}

type option func(*options) error

// New detects and generate machines elems
func New(ctx context.Context, cmdline string, opts ...option) (Machines, error) {
	log.Info(ctx, i18n.G("Building new machines list"))
	args := options{
		libzfs: &zfs.LibZFSAdapter{},
	}
	for _, o := range opts {
		if err := o(&args); err != nil {
			return Machines{}, fmt.Errorf(i18n.G("Couldn't apply option to server: %v"), err)
		}
	}

	z, err := zfs.New(ctx, zfs.WithLibZFS(args.libzfs))
	if err != nil {
		return Machines{}, fmt.Errorf(i18n.G("couldn't scan zfs filesystem"), err)
	}

	machines := Machines{
		all:     make(map[string]*Machine),
		cmdline: cmdline,
		z:       z,
	}
	if err := machines.refresh(ctx); err != nil {
		return Machines{}, err
	}

	return machines, nil
}

// Refresh reloads the list of machines after rescanning zfs datasets state from system
func (machines *Machines) Refresh(ctx context.Context) error {
	newMachines := Machines{
		all:     make(map[string]*Machine),
		cmdline: machines.cmdline,
		z:       machines.z,
	}
	if err := newMachines.z.Refresh(ctx); err != nil {
		return err
	}

	if err := newMachines.refresh(ctx); err != nil {
		return err
	}

	*machines = newMachines
	return nil
}

type originAndChildren struct {
	origin   string
	children []*zfs.Dataset
}

// refresh reloads the list of machines, based on already loaded zfs datasets state
func (machines *Machines) refresh(ctx context.Context) error {
	// We are going to transform the origin of datasets, get a copy first
	zDatasets := machines.z.Datasets()
	datasets := make([]*zfs.Dataset, 0, len(zDatasets))
	for i := range zDatasets {
		datasets = append(datasets, &zDatasets[i])
	}

	// Sort datasets so that children datasets are after their parents.
	sortedDataset := sortedDataset(datasets)
	sort.Sort(sortedDataset)

	// Resolve out to its root origin for /, /boot* and user datasets
	origins := resolveOrigin(ctx, []*zfs.Dataset(sortedDataset), "/")

	// First, set main datasets, then set clones
	mainDatasets := make([]zfs.Dataset, 0, len(sortedDataset))
	cloneDatasets := make([]zfs.Dataset, 0, len(sortedDataset))
	otherDatasets := make([]zfs.Dataset, 0, len(sortedDataset))
	for _, d := range sortedDataset {
		if origins[d.Name] == nil {
			otherDatasets = append(otherDatasets, *d)
			continue
		}
		if *origins[d.Name] == "" {
			mainDatasets = append(mainDatasets, *d)
		} else {
			cloneDatasets = append(cloneDatasets, *d)
		}
	}

	// First, handle system datasets (active for each machine and history) and return remaining ones.
	boots, flattenedUserDatas, persistents := machines.triageDatasets(ctx, append(append(mainDatasets, cloneDatasets...), otherDatasets...), origins)

	// Get a userdata map from parent to its children
	rootUserDatasets := getRootDatasets(ctx, flattenedUserDatas)

	var rootsOnlyUserDatasets []*zfs.Dataset
	for k := range rootUserDatasets {
		rootsOnlyUserDatasets = append(rootsOnlyUserDatasets, k)
	}
	originsUserDatasets := resolveOrigin(ctx, rootsOnlyUserDatasets, "")

	userdatas := make(map[*zfs.Dataset]originAndChildren)
	for k, ds := range rootUserDatasets {
		userdatas[k] = originAndChildren{
			origin:   *originsUserDatasets[k.Name],
			children: ds,
		}
	}

	// Attach to machine zsys boots and userdata non persisent datasets per machines before attaching persistents.
	// Same with children and history datasets.
	// We want reproducibility, so iterate to attach datasets in a given order.
	for _, k := range sortedMachineKeys(machines.all) {
		m := machines.all[k]
		m.attachRemainingDatasets(ctx, boots, persistents, userdatas)

		// attach to global list all system datasets of this machine
		machines.allSystemDatasets = append(machines.allSystemDatasets, m.SystemDatasets...)
		for _, k := range sortedStateKeys(m.History) {
			h := m.History[k]
			machines.allSystemDatasets = append(machines.allSystemDatasets, h.SystemDatasets...)
		}
	}

	for _, d := range flattenedUserDatas {
		if d.CanMount == "off" {
			continue
		}
		machines.allUsersDatasets = append(machines.allUsersDatasets, d)
	}

	// Append unlinked boot datasets to ensure we will switch to noauto everything
	machines.allSystemDatasets = appendIfNotPresent(machines.allSystemDatasets, boots, true)

	root, _ := bootParametersFromCmdline(machines.cmdline)
	m, _ := machines.findFromRoot(root)
	machines.current = m

	log.Debugf(ctx, i18n.G("current machines scanning layout:\n"+pp.Sprint(machines)))

	return nil
}

// triageDatasets attach main system datasets to machines and returns other types of datasets for later triage/attachment.
func (machines *Machines) triageDatasets(ctx context.Context, allDatasets []zfs.Dataset, origins map[string]*string) (boots, userdatas, persistents []*zfs.Dataset) {
	for _, d := range allDatasets {
		// we are taking the d address. Ensure we have a local variable that isn’t going to be reused
		d := d
		// Main active system dataset building up a machine
		m := newMachineFromDataset(d, origins[d.Name])
		if m != nil {
			machines.all[d.Name] = m
			continue
		}

		// Check for children, clones and snapshots
		if machines.attachSystemAndHistory(ctx, d, origins[d.Name]) {
			continue
		}

		// Starting from now, there is no children of system datasets

		// Extract boot datasets if any. We can't attach them directly with machines as if they are on another pool,
		// the machine is not necessiraly loaded yet.
		if strings.HasPrefix(d.Mountpoint, "/boot") {
			boots = append(boots, &d)
			continue
		}

		// Extract zsys user datasets if any. We can't attach them directly with machines as if they are on another pool,
		// the machine is not necessiraly loaded yet.
		if strings.Contains(strings.ToLower(d.Name), userdatasetsContainerName) {
			userdatas = append(userdatas, &d)
			continue
		}

		// At this point, it's either non zfs system or persistent dataset. Filters out canmount != "on" as nothing
		// will mount them.
		if d.CanMount != "on" {
			log.Debugf(ctx, i18n.G("ignoring %q: either an orphan clone or not a boot, user or system datasets and canmount isn't on"), d.Name)
			continue
		}

		// should be persistent datasets
		persistents = append(persistents, &d)
	}

	return boots, userdatas, persistents
}

// newMachineFromDataset returns a new machine if the given dataset is a main system one.
func newMachineFromDataset(d zfs.Dataset, origin *string) *Machine {
	// Register all zsys non cloned mountable / to a new machine
	if d.Mountpoint == "/" && d.CanMount != "off" && origin != nil && *origin == "" {
		m := Machine{
			State: State{
				ID:             d.Name,
				IsZsys:         d.BootFS,
				SystemDatasets: []*zfs.Dataset{&d},
			},
			Users:   make(map[string]map[string]UserState),
			History: make(map[string]*State),
		}
		// We don't want lastused to be 1970 in our golden files
		if d.LastUsed != 0 {
			lu := time.Unix(int64(d.LastUsed), 0)
			m.State.LastUsed = &lu
		}
		return &m
	}
	return nil
}

// attachSystemAndHistory identified if the given dataset is a system dataset (children of root one) or a history
// one. It creates and attach the states as needed.
// It returns ok if the dataset matches any machine and is attached.
func (machines *Machines) attachSystemAndHistory(ctx context.Context, d zfs.Dataset, origin *string) (ok bool) {
	for _, m := range machines.all {

		// Direct main machine state children
		if ok, err := isChild(m.ID, d); err != nil {
			log.Warningf(ctx, i18n.G("ignoring %q as couldn't assert if it's a child: ")+config.ErrorFormat, d.Name, err)
		} else if ok {
			m.SystemDatasets = append(m.SystemDatasets, &d)
			return true
		}

		// Clones or snapshot root dataset (origins points to origin dataset)
		if d.Mountpoint == "/" && d.CanMount != "off" && origin != nil && *origin == m.ID {
			m.History[d.Name] = &State{
				ID:             d.Name,
				IsZsys:         d.BootFS,
				SystemDatasets: []*zfs.Dataset{&d},
			}
			// We don't want lastused to be 1970 in our golden files
			if d.LastUsed != 0 {
				lu := time.Unix(int64(d.LastUsed), 0)
				m.History[d.Name].LastUsed = &lu
			}
			return true
		}

		// Clones or snapshot children
		for _, h := range m.History {
			if ok, err := isChild(h.ID, d); err != nil {
				log.Warningf(ctx, i18n.G("ignoring %q as couldn't assert if it's a child: ")+config.ErrorFormat, d.Name, err)
			} else if ok {
				h.SystemDatasets = append(h.SystemDatasets, &d)
				return true
			}
		}
	}

	return false
}

// attachRemainingDatasets attaches to machine boot, userdata and persistent datasets if they fit current machine.
func (m *Machine) attachRemainingDatasets(ctx context.Context, boots, persistents []*zfs.Dataset, userdatas map[*zfs.Dataset]originAndChildren) {
	e := strings.Split(m.ID, "/")
	// machineDatasetID is the main State dataset ID.
	machineDatasetID := e[len(e)-1]

	// Boot datasets
	var bootsDataset []*zfs.Dataset
	for _, d := range boots {
		if d.IsSnapshot {
			continue
		}
		// Matching base dataset name or subdataset of it.
		if strings.HasSuffix(d.Name, "/"+machineDatasetID) || strings.Contains(d.Name, "/"+machineDatasetID+"/") {
			bootsDataset = append(bootsDataset, d)
		}
	}
	m.SystemDatasets = append(m.SystemDatasets, bootsDataset...)

	// Userdata datasets. Don't base on machineID name as it's a tag on the dataset (the same userdataset can be
	// linked to multiple clones and systems).
	var userRootDatasets []*zfs.Dataset // Root user datasets for this machine
	for r := range userdatas {
		if r.IsSnapshot {
			continue
		}
		var valid bool
		// Only match datasets corresponding to the linked bootfs datasets (string slice separated by :)
		for _, bootfsDataset := range strings.Split(r.BootfsDatasets, ":") {
			if bootfsDataset == m.ID || strings.HasPrefix(r.BootfsDatasets, m.ID+"/") {
				valid = true
			}
		}
		if !valid {
			continue
		}
		userRootDatasets = append(userRootDatasets, r)
	}

	// Build whole user datasets history for this machine
	for r, dprop := range userdatas {
		// 1. Only take user datasets relative to this machine
		var relative bool
		for _, userRootDataset := range userRootDatasets {
			originUserRootDataset := userdatas[userRootDataset].origin
			if r == userRootDataset ||
				(dprop.origin != "" && dprop.origin == originUserRootDataset) ||
				(originUserRootDataset == "" && dprop.origin == userRootDataset.Name) ||
				(dprop.origin == "" && originUserRootDataset == r.Name) {
				relative = true
			}
		}
		if !relative {
			continue
		}

		// 2. Create user if we didn’t have it already
		t := strings.Split(filepath.Base(r.Name), "_")
		user := t[0]
		if len(t) > 1 {
			user = strings.Join(t[:len(t)-1], "_")
		}
		if m.Users[user] == nil {
			m.Users[user] = make(map[string]UserState)
		}

		// 3. Create dataset with state timestamp
		var timestamp string
		var isCurrent bool
		for _, userRootDataset := range userRootDatasets {
			if r == userRootDataset {
				isCurrent = true
			}
		}
		if isCurrent {
			timestamp = "current"
		} else {
			timestamp = strconv.Itoa(r.LastUsed)
		}

		if _, ok := m.Users[user][timestamp]; !ok {
			var delimiter = "_"
			if r.IsSnapshot {
				delimiter = "@"
			}
			t := strings.Split(filepath.Base(r.Name), delimiter)
			var id string
			if len(t) > 1 {
				id = t[len(t)-1]
			}

			m.Users[user][timestamp] = UserState{
				ID:       id,
				Datasets: append([]*zfs.Dataset{r}, dprop.children...),
			}
		}
	}

	// Attach current user datasets
	for user := range m.Users {
		userState, ok := m.Users[user]["current"]
		if !ok {
			continue
		}
		for _, d := range userState.Datasets {
			for _, bootfsDataset := range strings.Split(d.BootfsDatasets, ":") {
				if bootfsDataset == m.ID || strings.HasPrefix(d.BootfsDatasets, m.ID+"/") {
					m.UserDatasets = append(m.UserDatasets, d)
					break
				}
			}
		}
	}

	// Persistent datasets
	m.PersistentDatasets = persistents

	// Handle history now
	// We want reproducibility, so iterate to attach datasets in a given order.
	for _, k := range sortedStateKeys(m.History) {
		h := m.History[k]
		h.attachRemainingDatasetsForHistory(boots, persistents, m.Users)
	}
	// TODO: REMOVE
	m.Users = nil
}

// attachRemainingDatasetsForHistory attaches to a given history state boot, userdata and persistent datasets if they fit.
// It's similar to attachRemainingDatasets with some particular rules on snapshots.
func (h *State) attachRemainingDatasetsForHistory(boots, persistents []*zfs.Dataset, userdatas map[string]map[string]UserState) {
	e := strings.Split(h.ID, "/")
	// stateDatasetID may contain @snapshot, which we need to strip to test the suffix
	stateDatasetID := e[len(e)-1]
	var snapshot string
	if j := strings.LastIndex(stateDatasetID, "@"); j > 0 {
		snapshot = stateDatasetID[j+1:]
	}

	// Boot datasets
	var bootsDataset []*zfs.Dataset
	for _, d := range boots {
		if snapshot != "" {
			// Snapshots are not necessarily with a dataset ID matching its parent of dataset promotions, just match
			// its name.
			if strings.HasSuffix(d.Name, "@"+snapshot) {
				bootsDataset = append(bootsDataset, d)
				continue
			}
		}
		// For clones just match the base datasetname or its children.
		if strings.HasSuffix(d.Name, stateDatasetID) || strings.Contains(d.Name, "/"+stateDatasetID+"/") {
			bootsDataset = append(bootsDataset, d)
		}
	}
	h.SystemDatasets = append(h.SystemDatasets, bootsDataset...)

	var userDatasets []*zfs.Dataset
	for user := range userdatas {

		// snapshots are their own group identifier: all datasets are associated with this state
		if snapshot != "" {
			for _, userState := range userdatas[user] {
				if !userState.Datasets[0].IsSnapshot {
					continue
				}

				if userState.ID == snapshot {
					userDatasets = append(userDatasets, userState.Datasets...)
				}
			}
			continue
		}

		// If not a snapshot: we need to care about bootfsdatasets here as children or not all users of a given
		// user dataset state are not necesserally attached to this history state.
		for _, userState := range userdatas[user] {
			if userState.Datasets[0].IsSnapshot {
				continue
			}

			var found bool
			for _, d := range userState.Datasets {
				for _, bootfsDataset := range strings.Split(d.BootfsDatasets, ":") {
					if bootfsDataset == h.ID || strings.HasPrefix(d.BootfsDatasets, h.ID+"/") {
						userDatasets = append(userDatasets, d)
						found = true
						break
					}
				}
			}
			// Only take one matchable state for a given user
			if found {
				break
			}
		}
	}
	h.UserDatasets = append(h.UserDatasets, userDatasets...)

	// Persistent datasets
	h.PersistentDatasets = persistents
}

// isZsys returns if there is a current machine, and if it's the case, if it's zsys.
func (m *Machine) isZsys() bool {
	if m == nil {
		return false
	}
	return m.IsZsys
}

package archive

import (
	"errors"
	"fmt"

	"github.com/Knight1/tapemanager/internal/catalog"
	"github.com/Knight1/tapemanager/internal/ltfs"
	"github.com/Knight1/tapemanager/internal/manifest"
)

// VolumeRepairOptions configures PlanVolumeRepair.
type VolumeRepairOptions struct {
	TapeRoot string
	Catalog  *catalog.Catalog
	// ID names the cataloged tape to restore the record from. Without it
	// the tape is identified by its LTFS volume UUID or its records.
	ID string
}

// VolumeRepair is a planned rebuild of a tape's volume record. Nothing is
// written until Execute.
type VolumeRepair struct {
	Tape     catalog.Tape    // the cataloged tape the record is rebuilt from
	Volume   manifest.Volume // the record that will be written
	Damaged  bool            // a damaged record exists (kept as volume.json.damaged)
	Problem  string          // what is wrong with the current record
	Entries  int             // manifest entries readable on the tape
	Matched  bool            // the tape's entries agree with the catalog
	tapeRoot string
	cat      *catalog.Catalog
}

// PlanVolumeRepair finds out whether the volume record of the tape at
// opts.TapeRoot is damaged or missing, and which cataloged tape it belongs
// to. It refuses a tape whose record is fine, a tape without tapemgr
// records, and a tape whose records disagree with the cataloged tape: a
// wrong identity would make the catalog mix up two tapes.
func PlanVolumeRepair(opts VolumeRepairOptions) (*VolumeRepair, error) {
	if opts.Catalog == nil {
		return nil, errors.New("catalog is required")
	}
	if err := checkWritable(opts.TapeRoot); err != nil {
		return nil, err
	}
	tape, err := manifest.Open(opts.TapeRoot)
	if err != nil {
		return nil, err
	}
	defer tape.Close()

	r := &VolumeRepair{tapeRoot: opts.TapeRoot, cat: opts.Catalog}
	vol, verr := tape.Volume()
	switch {
	case verr != nil:
		r.Damaged, r.Problem = true, verr.Error()
	case vol != nil:
		return nil, fmt.Errorf("the volume record of this tape is fine (tape %s); nothing to repair", tapeLabel(catalog.Tape{Volume: *vol}))
	default:
		has, err := tape.HasRecords()
		if err != nil {
			return nil, err
		}
		if !has {
			return nil, errors.New("this tape has no tapemgr records; it gets a volume record with its first 'archive put'")
		}
		r.Problem = "the volume record is missing"
	}

	entries, _, err := tape.EntriesLenient()
	if err != nil {
		return nil, err
	}
	r.Entries = len(entries)
	uuid := ltfs.VolumeUUID(opts.TapeRoot)
	id := opts.ID
	if id == "" {
		if id = opts.Catalog.FindTape(uuid, entries); id == "" {
			return nil, errors.New("the tape could not be identified in the catalog; pass --id with its tape ID from 'tapemgr catalog tapes'")
		}
	}
	t, err := opts.Catalog.Tape(id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("tape %s is not in the catalog", id)
	}
	// The tape's own records must agree with what the catalog knows of
	// that tape. Without readable records only an explicit --id is
	// accepted.
	r.Matched = opts.Catalog.Matches(id, entries)
	switch {
	case len(entries) > 0 && !r.Matched:
		return nil, fmt.Errorf("the files on this tape do not match tape %s in the catalog; refusing to give it that identity", tapeLabel(*t))
	case len(entries) == 0 && opts.ID == "":
		return nil, errors.New("no readable records on the tape to confirm its identity; pass --id with its tape ID if you are sure")
	}
	if uuid != "" && t.LTFSUUID != "" && uuid != t.LTFSUUID {
		return nil, fmt.Errorf("the LTFS volume UUID of this tape (%s) differs from tape %s in the catalog (%s); refusing", uuid, tapeLabel(*t), t.LTFSUUID)
	}
	r.Tape = *t
	r.Volume = t.Volume
	if uuid != "" {
		r.Volume.LTFSUUID = uuid
	}
	return r, nil
}

// Execute writes the planned volume record, forces the LTFS index to tape
// and updates the catalog.
func (r *VolumeRepair) Execute() error {
	if err := checkWritable(r.tapeRoot); err != nil {
		return err
	}
	tape, err := manifest.Open(r.tapeRoot)
	if err != nil {
		return err
	}
	defer tape.Close()
	if err := tape.RepairVolume(r.Volume); err != nil {
		return err
	}
	if err := syncIndex(r.tapeRoot); err != nil {
		return err
	}
	if _, err := r.cat.Import(r.tapeRoot); err != nil {
		return fmt.Errorf("volume record written; updating the catalog failed (run 'tapemgr catalog import'): %w", err)
	}
	return nil
}

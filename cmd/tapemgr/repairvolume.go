package main

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/Knight1/tapemanager/internal/archive"
	"github.com/Knight1/tapemanager/internal/catalog"
)

// cmdRepairVolume rebuilds a damaged or missing volume record from the
// catalog. It only runs when asked and confirmed: the record is the tape's
// identity, so it is never rewritten on its own.
func cmdRepairVolume(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs, cf := newFlagSet("archive repair-volume", stderr)
	id := fs.String("id", "", "tape ID from 'tapemgr catalog tapes', if the tape cannot be identified on its own")
	yes := fs.Bool("yes", false, "write without asking for confirmation")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: tapemgr archive repair-volume [--id TAPE-ID] [--yes] [flags]")
		return exitUsage
	}
	if err := cf.checkTape(); err != nil {
		return fail(stderr, err)
	}
	if err := checkWriteProtect(cf.tape); err != nil {
		return fail(stderr, err)
	}
	cat, err := catalog.Open(cf.catalog)
	if err != nil {
		return fail(stderr, err)
	}
	plan, err := archive.PlanVolumeRepair(archive.VolumeRepairOptions{TapeRoot: cf.tape, Catalog: cat, ID: *id})
	if err != nil {
		return fail(stderr, err)
	}

	t := plan.Tape
	fmt.Fprintf(stdout, "Problem:   %s\n", plan.Problem)
	fmt.Fprintf(stdout, "Tape:      %s, %d files in the catalog", tapeName(&t.Volume), t.Files)
	if v := t.LastVerified(); v != nil {
		fmt.Fprintf(stdout, ", last verified %s", v.At.Format("2006-01-02"))
	}
	fmt.Fprintln(stdout)
	if plan.Matched {
		fmt.Fprintf(stdout, "Evidence:  the %d records on the tape match this tape in the catalog\n", plan.Entries)
	} else {
		fmt.Fprintln(stdout, "Evidence:  none on the tape (no readable records); chosen with --id")
	}
	fmt.Fprintf(stdout, "\nThis writes %s/.tapemgr/volume.json with ID %s", cf.tape, plan.Volume.ID)
	if plan.Volume.Label != "" {
		fmt.Fprintf(stdout, " and label %s", plan.Volume.Label)
	}
	fmt.Fprintln(stdout, ".")
	if plan.Damaged {
		fmt.Fprintln(stdout, "The damaged record is kept as .tapemgr/volume.json.damaged.")
	}
	if !*yes {
		fmt.Fprint(stdout, "\nContinue? [y/N] ")
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(stdout, "Aborted. Nothing was written.")
			return exitFailure
		}
	}
	if err := plan.Execute(); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "Volume record of tape %s rebuilt. Run 'tapemgr archive verify' to check the tape.\n", tapeName(&plan.Volume))
	return exitOK
}

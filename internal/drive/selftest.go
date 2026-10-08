package drive

import (
	"encoding/binary"
	"fmt"
	"time"
)

// SEND DIAGNOSTIC and the self-test results log page (SPC-4).
const (
	opSendDiagnostic = 0x1D
	PageSelfTest     = 0x10 // self-test results log page
)

// SelfTest selects which self-test the drive runs.
type SelfTest int

const (
	SelfTestShort    SelfTest = iota // quick, usually a minute or two
	SelfTestExtended                 // thorough, can take much longer
)

// code is the SEND DIAGNOSTIC self-test code (byte 1, bits 7..5). Only
// background codes are used: a background test returns the command at once
// and runs while the drive stays reachable, so every SCSI command stays
// short. A foreground test would hold the command for the whole test, and a
// command that does not return quickly can make an HBA reset the drive.
func (s SelfTest) code() byte {
	if s == SelfTestExtended {
		return 0x02 // background extended
	}
	return 0x01 // background short
}

// StartSelfTest asks the drive to begin a background self-test. It returns as
// soon as the drive accepts the request; the outcome appears later in the
// self-test results log page, which PollSelfTest waits for. ErrUnsupported
// means the drive does not offer background self-tests.
func StartSelfTest(d Device, s SelfTest) error {
	cdb := []byte{opSendDiagnostic, s.code() << 5, 0, 0, 0, 0}
	_, err := d.Do(cdb, DirNone, nil, shortTimeout)
	return err
}

// SelfTestResult is the newest entry of the self-test results log page.
type SelfTestResult struct {
	Function  byte   // the self-test code the drive ran
	Result    byte   // 0 passed, 0xF in progress, otherwise a failure or abort
	Segment   byte   // segment of the first failure, 0 if none or unknown
	Hours     uint16 // drive power-on hours when the test finished
	Key       byte   // sense key of a failure
	ASC, ASCQ byte
}

// InProgress reports whether a self-test is still running.
func (r SelfTestResult) InProgress() bool { return r.Result == 0x0F }

// Passed reports whether the most recent self-test completed without error.
func (r SelfTestResult) Passed() bool { return r.Result == 0x00 }

// Description explains the result value.
func (r SelfTestResult) Description() string {
	switch r.Result {
	case 0x00:
		return "passed"
	case 0x01:
		return "aborted by a self-test abort request"
	case 0x02:
		return "aborted by another method"
	case 0x03:
		return "did not complete (unknown error)"
	case 0x04:
		return "failed, failing segment unknown"
	case 0x05, 0x06, 0x07:
		return fmt.Sprintf("failed in segment %d", r.Segment)
	case 0x0F:
		return "in progress"
	}
	return fmt.Sprintf("result code 0x%x", r.Result)
}

// ReadSelfTest reads the newest self-test result. ok is false when the drive
// keeps no self-test result (the log page is absent or has no entry).
func ReadSelfTest(d Device) (r SelfTestResult, ok bool, err error) {
	lp, err := ReadLogPage(d, PageSelfTest)
	if err != nil {
		return SelfTestResult{}, false, err
	}
	// Parameter 0x0001 is the most recent test; entries run 0x0001..0x0014.
	p, ok := lp.Get(0x0001)
	if !ok || len(p.Value) < 15 {
		return SelfTestResult{}, false, nil
	}
	v := p.Value
	return SelfTestResult{
		Function: v[0] >> 5,
		Result:   v[0] & 0x0F,
		Segment:  v[1],
		Hours:    binary.BigEndian.Uint16(v[2:]),
		Key:      v[12] & 0x0F,
		ASC:      v[13],
		ASCQ:     v[14],
	}, true, nil
}

// PollSelfTest reads the self-test result until it is no longer in progress,
// waiting interval between reads and giving up after timeout. It returns the
// final result; ok is false if the drive never reported one.
func PollSelfTest(d Device, timeout, interval time.Duration) (r SelfTestResult, ok bool, err error) {
	deadline := time.Now().Add(timeout)
	for {
		r, ok, err = ReadSelfTest(d)
		if err != nil || !ok || !r.InProgress() {
			return r, ok, err
		}
		if time.Now().After(deadline) {
			return r, ok, fmt.Errorf("the self-test did not finish within %s; it keeps running on the drive", timeout)
		}
		time.Sleep(interval)
	}
}

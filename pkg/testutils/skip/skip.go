// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package skip

import (
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/util"
)

// SkippableTest is a testing.TB with Skip methods.
type SkippableTest interface {
	Skip(...interface{})
	Skipf(string, ...interface{})
}

// WithIssue skips this test, logging the given issue ID as the reason.
func WithIssue(t SkippableTest, githubIssueID int, args ...interface{}) {
	t.Skip(append([]interface{}{
		fmt.Sprintf("https://github.com/cockroachdb/cockroach/issue/%d", githubIssueID)},
		args...))
}

// IgnoreLint skips this test, explicitly marking it as not a test that
// should be tracked as a "skipped test" by external tools. You should use this
// if, for example, your test should only be run in Race mode.
func IgnoreLint(t SkippableTest, args ...interface{}) {
	t.Skip(args...)
}

// IgnoreLintf is like IgnoreLint, and it also takes a format string.
func IgnoreLintf(t SkippableTest, format string, args ...interface{}) {
	t.Skipf(format, args...)
}

// UnderRace skips this test if the race detector is enabled.
func UnderRace(t SkippableTest, args ...interface{}) {
	if util.RaceEnabled {
		t.Skip(append([]interface{}{"disabled under race"}, args...))
	}
}

// UnderRaceWithIssue skips this test if the race detector is enabled,
// logging the given issue ID as the reason.
func UnderRaceWithIssue(t SkippableTest, githubIssueID int, args ...interface{}) {
	if util.RaceEnabled {
		t.Skip(append([]interface{}{fmt.Sprintf(
			"disabled under race. issue: https://github.com/cockroachdb/cockroach/issue/%d", githubIssueID,
		)}, args...))
	}
}

// UnderShort skips this test if the -short flag is specified.
func UnderShort(t SkippableTest, args ...interface{}) {
	if testing.Short() {
		t.Skip(append([]interface{}{"disabled under -short"}, args...))
	}
}

// UnderStress skips this test when running under stress.
func UnderStress(t SkippableTest, args ...interface{}) {
	if NightlyStress() {
		t.Skip(append([]interface{}{"disabled under stress"}, args...))
	}
}

// UnderStressRace skips this test during stressrace runs, which are tests
// run under stress with the -race flag.
func UnderStressRace(t SkippableTest, args ...interface{}) {
	if NightlyStress() && util.RaceEnabled {
		t.Skip(append([]interface{}{"disabled under stressrace"}, args...))
	}
}

// UnderMetamorphic skips this test during metamorphic runs, which are tests
// run with the metamorphic build tag.
func UnderMetamorphic(t SkippableTest, args ...interface{}) {
	if util.MetamorphicBuild {
		t.Skip(append([]interface{}{"disabled under metamorphic"}, args...))
	}
}

package dind

import (
	"testing"

	"github.com/ephpm/ephemerd/pkg/jobcgroup"
)

// TestJobCgroupPrefixIsTheDindNamespacePrefix pins the coupling the job
// memory budget and the leaked-cgroup sweep both rely on: containerd puts a
// sibling in cgroup "/<namespace>/<id>", so the job parent cgroup is named
// after the job's dind namespace. If either prefix changes alone, siblings
// escape the job budget and the sweep stops matching what jobs leave behind.
func TestJobCgroupPrefixIsTheDindNamespacePrefix(t *testing.T) {
	if jobcgroup.Prefix != DindNamespacePrefix {
		t.Fatalf("jobcgroup.Prefix = %q, DindNamespacePrefix = %q: they must match", jobcgroup.Prefix, DindNamespacePrefix)
	}
	s := &Server{jobNamespace: DindNamespacePrefix + "job-1"}
	if "/"+s.jobNamespace != jobcgroup.Dir("job-1") {
		t.Fatalf("job namespace %q does not name the job cgroup %q", s.jobNamespace, jobcgroup.Dir("job-1"))
	}
}

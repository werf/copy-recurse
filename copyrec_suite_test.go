package copyrec_test

import (
	"syscall"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/werf/logboek"
	"github.com/werf/logboek/pkg/level"
)

func TestCopyRecurse(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "CopyRecurse Suite")
}

var _ = BeforeSuite(func() {
	logboek.SetAcceptedLevel(level.Debug)
	syscall.Umask(0o022)
})

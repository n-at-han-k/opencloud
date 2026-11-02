package hls_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestHLS(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "HLS Suite")
}

package oneagent

import (
	"errors"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"testing"
)

func TestGetNextPodToRestart(t *testing.T) {
	pods := []corev1.Pod{
		{Status: corev1.PodStatus{HostIP: "127.0.0.1"}},
		{Status: corev1.PodStatus{HostIP: "127.0.0.2"}},
		{Status: corev1.PodStatus{HostIP: "127.0.0.3"}},
	}

	{
		dtc := new(MyDynatraceClient)
		dtc.On("GetVersionForIp", "127.0.0.1").Return("1.2.3", nil)
		dtc.On("GetVersionForIp", "127.0.0.2").Return("0.1.2", nil)
		dtc.On("GetVersionForIp", "127.0.0.3").Return("", errors.New("n/a"))

		doomed, version, err := getNextPodToRestart(pods, dtc, "1.2.3")

		assert.Equalf(t, *doomed, pods[1], "first pods to restart")
		assert.Equalf(t, version, "0.1.2", "must be actual version")
		assert.Nilf(t, err, "err must be null")
	}
	{
		dtc := new(MyDynatraceClient)
		dtc.On("GetVersionForIp", "127.0.0.1").Return("1.2.3", nil)
		dtc.On("GetVersionForIp", "127.0.0.2").Return("", errors.New("n/a"))
		dtc.On("GetVersionForIp", "127.0.0.3").Return("1.2.3", nil)

		doomed, version, err := getNextPodToRestart(pods, dtc, "1.2.3")

		assert.Nilf(t, doomed, "doomed must be null")
		assert.Equalf(t, version, "", "must be empty")
		assert.Errorf(t, err, "n/a", "err must be null")
	}
}

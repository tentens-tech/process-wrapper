package process

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	log "github.com/sirupsen/logrus"
	"io"
	"net/http"
	"os"
	"time"
)

const (
	PodInfoURI                = "https://kubernetes.default.svc.cluster.local/api/v1/namespaces/%v/pods/%v"
	DefaultServiceAccountPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	UpdateStatusPeriod        = 5
)

type PodStatus struct {
	cancel context.CancelFunc
	Status struct {
		ContainerStatuses []ContainerStatuses `json:"containerStatuses"`
	} `json:"status"`
}

type ContainerStatuses struct {
	Name         string                    `json:"name"`
	State        map[string]ContainerState `json:"state"`
	Ready        bool                      `json:"ready"`
	RestartCount int                       `json:"restartCount"`
	Image        string                    `json:"image"`
	ImageID      string                    `json:"imageID"`
	ContainerID  string                    `json:"containerID"`
	Started      bool                      `json:"started"`
}

type ContainerState struct {
	ExitCode    int       `json:"exitCode"`
	Reason      string    `json:"reason"`
	StartedAt   time.Time `json:"startedAt"`
	FinishedAt  time.Time `json:"finishedAt"`
	ContainerID string    `json:"containerID"`
}

func NewPodStatus(cancel context.CancelFunc) *PodStatus {
	podStatus := &PodStatus{cancel: cancel}
	go podStatus.ReadStatusController()
	return podStatus
}

func (p *PodStatus) ReadStatusController() {
	switch os.Getenv("HANDLE_TERMINATION") {
	case "1", "true", "enabled":
		var err error
		log.Printf("Start pod status controller")
		for {
			err = p.ReadStatus()
			if err != nil {
				log.Errorf("Read pod status error, %v", err)
			}
			time.Sleep(time.Second * UpdateStatusPeriod)
		}
	}
}

func (p *PodStatus) ReadStatus() error {
	client := &http.Client{
		Timeout: time.Second * 10,
		Transport: &http.Transport{
			//TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: false,
				MinVersion:         tls.VersionTLS12, // Set the minimum TLS version to TLS 1.2
			},
		},
	}
	podNamespace := os.Getenv("POD_NAMESPACE")
	if podNamespace == "" {
		return fmt.Errorf("POD_NAMESPACE env isn't set")
	}
	uri := fmt.Sprintf(PodInfoURI, podNamespace, os.Getenv("HOSTNAME"))
	log.Debugf("Get pod status %v", uri)
	token, err := os.ReadFile(DefaultServiceAccountPath)
	if err != nil {
		log.Errorf("Cannot read kubernetes token, %v", err)
		return err
	}

	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return err
	}
	req.Header.Add("Authorization", fmt.Sprintf("Bearer %v", string(token)))
	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	defer func() {
		if e := resp.Body.Close(); e != nil {
			log.Errorf("%v", e)
		}
	}()

	err = json.Unmarshal(data, p)
	if err != nil {
		log.Errorf("Pod status: %v", string(data))
		return err
	}
	log.Debugf("Pod status: %+v", p.Status.ContainerStatuses)
	for _, s := range p.Status.ContainerStatuses {
		log.Debugf("Container %v status: %+v", s.Name, s.State)
		state, ok := s.State["terminated"]
		if ok {
			log.Warnf("Main container is terminated, %v state %+v", s.Name, state)
			if state.Reason == "Completed" && state.ExitCode < 1 {
				log.Warnf("Main conainer is completed, terminate process-wrapper")
				p.cancel()
			}
		}
	}

	return err
}

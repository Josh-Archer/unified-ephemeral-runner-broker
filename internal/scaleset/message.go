package scaleset

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const (
	// MessageTypeJobMessages is the only batch type the message queue returns.
	MessageTypeJobMessages = "RunnerScaleSetJobMessages"

	MessageTypeJobAvailable = "JobAvailable"
	MessageTypeJobAssigned  = "JobAssigned"
	MessageTypeJobStarted   = "JobStarted"
	MessageTypeJobCompleted = "JobCompleted"

	// HeaderScaleSetMaxCapacity tells GitHub how many runners this listener can
	// take. It is X-ScaleSetMaxCapacity on every poll.
	HeaderScaleSetMaxCapacity = "X-ScaleSetMaxCapacity"

	// MessagePollTimeout is the per-attempt ceiling for one long poll. The
	// service holds an empty poll for about 50 seconds.
	MessagePollTimeout = 2 * time.Minute
)

// Statistic is the scale-set snapshot carried on every session and message.
// TotalAssignedJobs is the demand signal: jobs waiting plus jobs already running.
type Statistic struct {
	TotalAvailableJobs     int `json:"totalAvailableJobs"`
	TotalAcquiredJobs      int `json:"totalAcquiredJobs"`
	TotalAssignedJobs      int `json:"totalAssignedJobs"`
	TotalRunningJobs       int `json:"totalRunningJobs"`
	TotalRegisteredRunners int `json:"totalRegisteredRunners"`
	TotalBusyRunners       int `json:"totalBusyRunners"`
	TotalIdleRunners       int `json:"totalIdleRunners"`
}

// Session is the message-queue session the admin API returns. The listener
// consumes it; this package does not create it.
type Session struct {
	SessionID               string     `json:"sessionId"`
	MessageQueueURL         string     `json:"messageQueueUrl"`
	MessageQueueAccessToken string     `json:"messageQueueAccessToken"`
	Statistics              *Statistic `json:"statistics"`
}

// Message is one poll result. A nil Message is an empty long poll (HTTP 202).
type Message struct {
	MessageID            int
	Statistics           *Statistic
	JobAvailableMessages []*JobAvailable
	JobAssignedMessages  []*JobAssigned
	JobStartedMessages   []*JobStarted
	JobCompletedMessages []*JobCompleted
}

// JobAvailable is a job the scale set may acquire. Acquiring is left off until
// runners register with JIT config; acquiring without that strands the job.
type JobAvailable struct {
	JobMessageBase
}

// JobAssigned is a job GitHub has assigned to the scale set.
type JobAssigned struct {
	JobMessageBase
}

// JobStarted is a job a runner has picked up.
type JobStarted struct {
	RunnerID   int    `json:"runnerId"`
	RunnerName string `json:"runnerName"`
	JobMessageBase
}

// JobCompleted is a job a runner has finished.
type JobCompleted struct {
	Result     string `json:"result"`
	RunnerID   int    `json:"runnerId"`
	RunnerName string `json:"runnerName"`
	JobMessageBase
}

// JobMessageBase is the common job payload inside a scale-set message batch.
type JobMessageBase struct {
	MessageType        string    `json:"messageType"`
	RunnerRequestID    int64     `json:"runnerRequestId"`
	RepositoryName     string    `json:"repositoryName"`
	OwnerName          string    `json:"ownerName"`
	JobID              string    `json:"jobId"`
	JobWorkflowRef     string    `json:"jobWorkflowRef"`
	JobDisplayName     string    `json:"jobDisplayName"`
	WorkflowRunID      int64     `json:"workflowRunId"`
	RequestLabels      []string  `json:"requestLabels"`
	QueueTime          time.Time `json:"queueTime"`
	ScaleSetAssignTime time.Time `json:"scaleSetAssignTime"`
}

type messageResponse struct {
	MessageID   int        `json:"messageId"`
	MessageType string     `json:"messageType"`
	Body        string     `json:"body"`
	Statistics  *Statistic `json:"statistics"`
}

type jobMessageType struct {
	MessageType string `json:"messageType"`
}

// ParseMessage decodes one HTTP 200 message-queue body. The body field is a
// JSON string containing an array of job messages, matching actions/scaleset.
func ParseMessage(r io.Reader) (*Message, error) {
	var raw messageResponse
	if err := json.NewDecoder(io.LimitReader(r, 2<<20)).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode scale set message: %w", err)
	}
	if raw.MessageType != MessageTypeJobMessages {
		return nil, fmt.Errorf("unsupported scale set message type %q", raw.MessageType)
	}
	msg := &Message{MessageID: raw.MessageID, Statistics: raw.Statistics}
	if len(raw.Body) == 0 {
		return msg, nil
	}
	var batched []json.RawMessage
	if err := json.Unmarshal([]byte(raw.Body), &batched); err != nil {
		return nil, fmt.Errorf("decode scale set message batch: %w", err)
	}
	for _, item := range batched {
		var kind jobMessageType
		if err := json.Unmarshal(item, &kind); err != nil {
			return nil, fmt.Errorf("decode scale set job message type: %w", err)
		}
		switch kind.MessageType {
		case MessageTypeJobAvailable:
			var job JobAvailable
			if err := json.Unmarshal(item, &job); err != nil {
				return nil, fmt.Errorf("decode job available: %w", err)
			}
			msg.JobAvailableMessages = append(msg.JobAvailableMessages, &job)
		case MessageTypeJobAssigned:
			var job JobAssigned
			if err := json.Unmarshal(item, &job); err != nil {
				return nil, fmt.Errorf("decode job assigned: %w", err)
			}
			msg.JobAssignedMessages = append(msg.JobAssignedMessages, &job)
		case MessageTypeJobStarted:
			var job JobStarted
			if err := json.Unmarshal(item, &job); err != nil {
				return nil, fmt.Errorf("decode job started: %w", err)
			}
			msg.JobStartedMessages = append(msg.JobStartedMessages, &job)
		case MessageTypeJobCompleted:
			var job JobCompleted
			if err := json.Unmarshal(item, &job); err != nil {
				return nil, fmt.Errorf("decode job completed: %w", err)
			}
			msg.JobCompletedMessages = append(msg.JobCompletedMessages, &job)
		}
	}
	return msg, nil
}

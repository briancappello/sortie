package huly

import (
	"encoding/json"
	"time"
)

// Huly class, space and attribute ids the adapter reads and writes.
const (
	classIssue          = "tracker:class:Issue"
	classIssueStatus    = "tracker:class:IssueStatus"
	classProject        = "tracker:class:Project"
	classProjectType    = "task:class:ProjectType"
	classTaskType       = "task:class:TaskType"
	classPerson         = "contact:class:Person"
	classSocialIdentity = "contact:class:SocialIdentity"
	classChatMessage    = "chunter:class:ChatMessage"
	classTagElement     = "tags:class:TagElement"
	classTagReference   = "tags:class:TagReference"
	classTxCreateDoc    = "core:class:TxCreateDoc"
	classTxUpdateDoc    = "core:class:TxUpdateDoc"
	classTxRemoveDoc    = "core:class:TxRemoveDoc"

	spaceTx        = "core:space:Tx"
	spaceWorkspace = "core:space:Workspace"

	noParent         = "tracker:ids:NoParent"
	labelCategory    = "tracker:category:Other"
	collectionLabels = "labels"
	collectionNotes  = "comments"
)

type hulyProject struct {
	ID                 string `json:"_id"`
	Identifier         string `json:"identifier"`
	Name               string `json:"name"`
	Type               string `json:"type"`
	DefaultIssueStatus string `json:"defaultIssueStatus"`
}

type hulyProjectType struct {
	ID       string `json:"_id"`
	Name     string `json:"name"`
	Statuses []struct {
		ID string `json:"_id"`
	} `json:"statuses"`
}

type hulyStatus struct {
	ID       string `json:"_id"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

type relatedDoc struct {
	ID    string `json:"_id"`
	Class string `json:"_class"`
}

type parentInfo struct {
	ParentID    string `json:"parentId"`
	ParentTitle string `json:"parentTitle"`
	Space       string `json:"space"`
	Identifier  string `json:"identifier"`
}

type hulyIssue struct {
	ID          string       `json:"_id"`
	Space       string       `json:"space"`
	Identifier  string       `json:"identifier"`
	Title       string       `json:"title"`
	Description *string      `json:"description"`
	Status      string       `json:"status"`
	Priority    *float64     `json:"priority"`
	Assignee    *string      `json:"assignee"`
	AttachedTo  string       `json:"attachedTo"`
	Parents     []parentInfo `json:"parents"`
	BlockedBy   []relatedDoc `json:"blockedBy"`
	CreatedOn   *int64       `json:"createdOn"`
	ModifiedOn  int64        `json:"modifiedOn"`

	Kind           string `json:"kind"`
	Rank           string `json:"rank"`
	PlanKey        string `json:"sortiePlanKey"`
	PlanBodyDigest string `json:"sortiePlanBodyDigest"`
}

type hulyPerson struct {
	ID   string `json:"_id"`
	Name string `json:"name"`
}

type hulySocialIdentity struct {
	ID         string `json:"_id"`
	AttachedTo string `json:"attachedTo"`
	Value      string `json:"value"`
}

type hulyMessage struct {
	ID         string `json:"_id"`
	Message    string `json:"message"`
	CreatedBy  string `json:"createdBy"`
	ModifiedBy string `json:"modifiedBy"`
	CreatedOn  *int64 `json:"createdOn"`
	ModifiedOn int64  `json:"modifiedOn"`
}

type hulyTagElement struct {
	ID    string `json:"_id"`
	Title string `json:"title"`
	Color int    `json:"color"`
}

type hulyTagReference struct {
	ID    string `json:"_id"`
	Space string `json:"space"`
	Tag   string `json:"tag"`
	Title string `json:"title"`
}

// decodeAll decodes every raw document into T.
func decodeAll[T any](raws []json.RawMessage) ([]T, error) {
	out := make([]T, 0, len(raws))
	for _, raw := range raws {
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, payloadError("decode document", err)
		}
		out = append(out, v)
	}
	return out, nil
}

// isoTime renders a Huly millisecond timestamp as RFC 3339 UTC, and the
// empty string for zero.
func isoTime(ms int64) string {
	if ms == 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

// created returns the creation time, falling back to the modification time
// for documents created before Huly recorded createdOn.
func created(createdOn *int64, modifiedOn int64) int64 {
	if createdOn != nil && *createdOn != 0 {
		return *createdOn
	}
	return modifiedOn
}

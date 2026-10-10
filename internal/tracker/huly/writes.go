package huly

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/issuekit"
	"github.com/sortie-ai/sortie/internal/tracker/huly/markup"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
)

// TransitionIssue sets the issue's status to the project status named
// targetState, compared case-insensitively. Huly has no transition graph,
// so any status can follow any other.
func (a *HulyAdapter) TransitionIssue(ctx context.Context, issueID, targetState string) error {
	return trackermetrics.Track(a.recorder(), "transition", func() error {
		ids := a.statusIDs([]string{targetState})
		if len(ids) == 0 {
			return &domain.TrackerError{
				Kind:    domain.ErrTrackerPayload,
				Message: fmt.Sprintf("huly: state %q not in project %s", targetState, a.project.Identifier),
			}
		}

		issue, err := a.findIssue(ctx, issueID)
		if err != nil {
			return err
		}
		if issue.Status == ids[0] {
			return nil
		}

		return a.rpc.Tx(ctx, a.updateTx(classIssue, issue.Space, issue.ID, map[string]any{"status": ids[0]}))
	})
}

// CommentIssue posts text as a comment, one paragraph per line.
func (a *HulyAdapter) CommentIssue(ctx context.Context, issueID, text string) error {
	return trackermetrics.Track(a.recorder(), "comment", func() error {
		return a.postComment(ctx, issueID, markup.FromText(text))
	})
}

// CommentIssueWithLiteral posts text followed by literal in a code block.
func (a *HulyAdapter) CommentIssueWithLiteral(ctx context.Context, issueID, text, literal string) error {
	return trackermetrics.Track(a.recorder(), "comment", func() error {
		return a.postComment(ctx, issueID, markup.FromTextWithLiteral(text, literal))
	})
}

func (a *HulyAdapter) postComment(ctx context.Context, issueID, message string) error {
	issue, err := a.findIssue(ctx, issueID)
	if err != nil {
		return err
	}

	tx := a.createTx(classChatMessage, issue.Space, generateID(), map[string]any{"message": message})
	attach(tx, issue.ID, classIssue, collectionNotes)
	return a.rpc.Tx(ctx, tx)
}

// AddLabel attaches the label to the issue, creating the workspace label
// when none of that name exists. Existing labels are untouched.
func (a *HulyAdapter) AddLabel(ctx context.Context, issueID, label string) error {
	return trackermetrics.Track(a.recorder(), "add_label", func() error {
		issue, err := a.findIssue(ctx, issueID)
		if err != nil && !isBlank(label) {
			return err
		}
		return issuekit.AddIssueLabel(ctx, label, a.labelOps(issue))
	})
}

// RemoveLabel detaches every label of the issue that names label.
func (a *HulyAdapter) RemoveLabel(ctx context.Context, issueID, label string) error {
	return trackermetrics.Track(a.recorder(), "remove_label", func() error {
		issue, err := a.findIssue(ctx, issueID)
		if err != nil && !isBlank(label) {
			return err
		}
		return issuekit.RemoveIssueLabel(ctx, label, a.labelOps(issue))
	})
}

func isBlank(label string) bool {
	return strings.TrimSpace(label) == ""
}

func (a *HulyAdapter) labelOps(issue hulyIssue) issuekit.IssueLabelOps {
	return issuekit.IssueLabelOps{
		Read: func(ctx context.Context) ([]issuekit.IssueLabel, error) {
			refs, err := a.issueLabelRefs(ctx, issue.ID)
			if err != nil {
				return nil, err
			}
			labels := make([]issuekit.IssueLabel, 0, len(refs))
			for _, r := range refs {
				labels = append(labels, issuekit.IssueLabel{Name: r.Title, ID: r.ID})
			}
			return labels, nil
		},
		Add: func(ctx context.Context, label string) (issuekit.LabelWrite, error) {
			refs, err := a.issueLabelRefs(ctx, issue.ID)
			if err != nil {
				return issuekit.LabelWrite{}, err
			}
			for _, r := range refs {
				if issuekit.SameLabel(r.Title, label) {
					return issuekit.LabelWrite{}, nil
				}
			}

			tag, err := a.resolveOrCreateTag(ctx, label)
			if err != nil {
				return issuekit.LabelWrite{}, err
			}

			tx := a.createTx(classTagReference, issue.Space, generateID(), map[string]any{
				"tag":   tag.ID,
				"title": tag.Title,
				"color": tag.Color,
			})
			attach(tx, issue.ID, classIssue, collectionLabels)
			return issuekit.LabelWrite{}, a.rpc.Tx(ctx, tx)
		},
		Remove: func(ctx context.Context, labels []issuekit.IssueLabel) (issuekit.LabelWrite, error) {
			for _, l := range labels {
				tx := a.removeTx(classTagReference, issue.Space, l.ID)
				attach(tx, issue.ID, classIssue, collectionLabels)
				if err := a.rpc.Tx(ctx, tx); err != nil {
					return issuekit.LabelWrite{}, err
				}
			}
			return issuekit.LabelWrite{}, nil
		},
	}
}

func (a *HulyAdapter) issueLabelRefs(ctx context.Context, issueID string) ([]hulyTagReference, error) {
	raws, err := a.rpc.FindAll(ctx, classTagReference, map[string]any{
		"attachedTo": issueID,
		"collection": collectionLabels,
	}, nil)
	if err != nil {
		return nil, err
	}
	return decodeAll[hulyTagReference](raws)
}

// resolveOrCreateTag returns the issue label named label, compared
// case-insensitively, creating it when the workspace has none.
func (a *HulyAdapter) resolveOrCreateTag(ctx context.Context, label string) (hulyTagElement, error) {
	raws, err := a.rpc.FindAll(ctx, classTagElement, map[string]any{"targetClass": classIssue}, nil)
	if err != nil {
		return hulyTagElement{}, err
	}
	tags, err := decodeAll[hulyTagElement](raws)
	if err != nil {
		return hulyTagElement{}, err
	}
	for _, t := range tags {
		if strings.EqualFold(t.Title, label) {
			return t, nil
		}
	}

	tag := hulyTagElement{ID: generateID(), Title: label, Color: labelColor(label)}
	err = a.rpc.Tx(ctx, a.createTx(classTagElement, spaceWorkspace, tag.ID, map[string]any{
		"title":       tag.Title,
		"description": "",
		"targetClass": classIssue,
		"color":       tag.Color,
		"category":    labelCategory,
	}))
	return tag, err
}

// labelColor picks a stable palette index from the label text, as Huly's
// own label picker does.
func labelColor(label string) int {
	sum := 0
	for _, r := range label {
		sum += int(r)
	}
	return sum % 20
}

func (a *HulyAdapter) txBase(class, txClass, space, objectID string) map[string]any {
	return map[string]any{
		"_id":         generateID(),
		"_class":      txClass,
		"space":       spaceTx,
		"objectId":    objectID,
		"objectClass": class,
		"objectSpace": space,
		"modifiedOn":  time.Now().UnixMilli(),
		"modifiedBy":  a.socialID,
	}
}

func (a *HulyAdapter) createTx(class, space, objectID string, attributes map[string]any) map[string]any {
	tx := a.txBase(class, classTxCreateDoc, space, objectID)
	tx["createdBy"] = a.socialID
	tx["attributes"] = attributes
	return tx
}

func (a *HulyAdapter) updateTx(class, space, objectID string, operations map[string]any) map[string]any {
	tx := a.txBase(class, classTxUpdateDoc, space, objectID)
	tx["operations"] = operations
	return tx
}

func (a *HulyAdapter) removeTx(class, space, objectID string) map[string]any {
	return a.txBase(class, classTxRemoveDoc, space, objectID)
}

// attach makes tx a collection transaction on the parent document.
func attach(tx map[string]any, parentID, parentClass, collection string) {
	tx["attachedTo"] = parentID
	tx["attachedToClass"] = parentClass
	tx["collection"] = collection
}

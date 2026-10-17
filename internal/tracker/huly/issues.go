package huly

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
	"github.com/sortie-ai/sortie/internal/issuekit"
	"github.com/sortie-ai/sortie/internal/tracker/huly/markup"
	"github.com/sortie-ai/sortie/internal/trackermetrics"
)

// FetchCandidateIssues returns the project's issues in the configured
// active states, without comments and descriptions.
func (a *HulyAdapter) FetchCandidateIssues(ctx context.Context) ([]domain.Issue, error) {
	var issues []domain.Issue
	err := trackermetrics.Track(a.recorder(), "fetch_candidates", func() error {
		var err error
		issues, err = a.fetchByStates(ctx, a.TrackerStates().Active)
		return err
	})
	return issues, err
}

// FetchIssuesByStates returns the project's issues in the named states,
// compared case-insensitively.
func (a *HulyAdapter) FetchIssuesByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	var issues []domain.Issue
	err := trackermetrics.Track(a.recorder(), "fetch_by_states", func() error {
		var err error
		issues, err = a.fetchByStates(ctx, states)
		return err
	})
	return issues, err
}

func (a *HulyAdapter) fetchByStates(ctx context.Context, states []string) ([]domain.Issue, error) {
	ids := a.statusIDs(states)
	if len(ids) == 0 {
		return []domain.Issue{}, nil
	}

	raws, err := a.rpc.FindAll(ctx, classIssue, map[string]any{
		"space":  idIn([]string{a.project.ID}),
		"status": idIn(ids),
	}, map[string]any{"sort": map[string]any{"createdOn": 1}})
	if err != nil {
		return nil, err
	}
	issues, err := decodeAll[hulyIssue](raws)
	if err != nil {
		return nil, err
	}
	return a.normalize(ctx, issues)
}

// FetchIssueByID returns one issue with its description and comments. The
// issueID is a Huly document id or a human identifier such as "HULY-12".
func (a *HulyAdapter) FetchIssueByID(ctx context.Context, issueID string) (domain.Issue, error) {
	var out domain.Issue
	err := trackermetrics.Track(a.recorder(), "fetch_issue", func() error {
		raw, err := a.findIssue(ctx, issueID)
		if err != nil {
			return err
		}

		normalized, err := a.normalize(ctx, []hulyIssue{raw})
		if err != nil {
			return err
		}
		out = normalized[0]

		if raw.Description != nil && *raw.Description != "" {
			serialized, err := a.rpc.GetMarkup(ctx, classIssue, raw.ID, "description", *raw.Description)
			if err != nil {
				return err
			}
			out.Description = markup.ToMarkdown(serialized)
		}

		comments, err := a.comments(ctx, raw.ID)
		if err != nil {
			return err
		}
		out.Comments = comments
		return nil
	})
	return out, err
}

// FetchIssueComments returns the issue's comments in ascending creation
// order.
func (a *HulyAdapter) FetchIssueComments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	var out []domain.Comment
	err := trackermetrics.Track(a.recorder(), "fetch_comments", func() error {
		raw, err := a.findIssue(ctx, issueID)
		if err != nil {
			return err
		}
		out, err = a.comments(ctx, raw.ID)
		return err
	})
	return out, err
}

// FetchIssueStatesByIDs returns the current state name of each issue found.
func (a *HulyAdapter) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) (map[string]string, error) {
	var out map[string]string
	err := trackermetrics.Track(a.recorder(), "fetch_states_by_ids", func() error {
		var err error
		out, err = a.statesBy(ctx, "_id", issueIDs, func(i hulyIssue) string { return i.ID })
		return err
	})
	return out, err
}

// FetchIssueStatesByIdentifiers returns the current state name of each
// issue found, keyed by identifier.
func (a *HulyAdapter) FetchIssueStatesByIdentifiers(ctx context.Context, identifiers []string) (map[string]string, error) {
	var out map[string]string
	err := trackermetrics.Track(a.recorder(), "fetch_states_by_identifiers", func() error {
		var err error
		out, err = a.statesBy(ctx, "identifier", identifiers, func(i hulyIssue) string { return i.Identifier })
		return err
	})
	return out, err
}

func (a *HulyAdapter) statesBy(ctx context.Context, field string, keys []string, key func(hulyIssue) string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	keys = dedupe(keys)
	if len(keys) == 0 {
		return out, nil
	}

	raws, err := a.rpc.FindAll(ctx, classIssue, map[string]any{field: idIn(keys)}, map[string]any{
		"projection": map[string]any{"_id": 1, "identifier": 1, "status": 1},
	})
	if err != nil {
		return nil, err
	}
	issues, err := decodeAll[hulyIssue](raws)
	if err != nil {
		return nil, err
	}

	statusIDs := make([]string, 0, len(issues))
	for _, i := range issues {
		statusIDs = append(statusIDs, i.Status)
	}
	statuses, err := a.lookupStatuses(ctx, statusIDs)
	if err != nil {
		return nil, err
	}

	for _, i := range issues {
		out[key(i)] = statuses[i.Status].Name
	}
	return out, nil
}

// findIssue reads one issue by document id, or by identifier when the
// argument has the PROJECT-N shape.
func (a *HulyAdapter) findIssue(ctx context.Context, issueID string) (hulyIssue, error) {
	field := "_id"
	if looksLikeIdentifier(issueID) {
		field = "identifier"
	}

	// Huly omits a field matched by equality from the documents it
	// returns, so match by $in to keep the identifier and id in the result.
	raws, err := a.rpc.FindAll(ctx, classIssue, map[string]any{field: idIn([]string{issueID})}, map[string]any{"limit": 1})
	if err != nil {
		return hulyIssue{}, err
	}
	issues, err := decodeAll[hulyIssue](raws)
	if err != nil {
		return hulyIssue{}, err
	}
	if len(issues) == 0 {
		return hulyIssue{}, &domain.TrackerError{
			Kind:    domain.ErrTrackerNotFound,
			Message: fmt.Sprintf("huly: issue %s not found", issueID),
		}
	}
	return issues[0], nil
}

// looksLikeIdentifier reports whether s has the PROJECT-N shape of a Huly
// issue identifier rather than a document id.
func looksLikeIdentifier(s string) bool {
	prefix, number, ok := strings.Cut(s, "-")
	if !ok || prefix == "" || number == "" {
		return false
	}
	for _, r := range number {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// normalize converts issues to domain issues, resolving status names,
// assignee names, labels, parents and blockers in batched reads.
// Descriptions and comments are left empty.
func (a *HulyAdapter) normalize(ctx context.Context, issues []hulyIssue) ([]domain.Issue, error) {
	if len(issues) == 0 {
		return []domain.Issue{}, nil
	}

	var statusIDs, personIDs, issueIDs, blockerIDs []string
	for _, i := range issues {
		statusIDs = append(statusIDs, i.Status)
		issueIDs = append(issueIDs, i.ID)
		if i.Assignee != nil {
			personIDs = append(personIDs, *i.Assignee)
		}
		for _, b := range i.BlockedBy {
			if b.Class == "" || b.Class == classIssue {
				blockerIDs = append(blockerIDs, b.ID)
			}
		}
	}

	blockers := map[string]hulyIssue{}
	if ids := dedupe(blockerIDs); len(ids) > 0 {
		raws, err := a.rpc.FindAll(ctx, classIssue, map[string]any{"_id": idIn(ids)}, map[string]any{
			"projection": map[string]any{"_id": 1, "identifier": 1, "status": 1, attrPlanKey: 1},
		})
		if err != nil {
			return nil, err
		}
		found, err := decodeAll[hulyIssue](raws)
		if err != nil {
			return nil, err
		}
		for _, b := range found {
			blockers[b.ID] = b
			statusIDs = append(statusIDs, b.Status)
		}
	}

	statuses, err := a.lookupStatuses(ctx, dedupe(statusIDs))
	if err != nil {
		return nil, err
	}

	people, err := a.personNames(ctx, dedupe(personIDs))
	if err != nil {
		return nil, err
	}

	labels, err := a.labelsByIssue(ctx, issueIDs)
	if err != nil {
		return nil, err
	}

	out := make([]domain.Issue, 0, len(issues))
	for _, i := range issues {
		issue := domain.Issue{
			ID:         i.ID,
			Identifier: i.Identifier,
			Title:      i.Title,
			Priority:   normalizePriority(i.Priority),
			State:      statuses[i.Status].Name,
			URL:        a.issueURLBase + i.Identifier,
			Labels:     issuekit.NormalizeLabels(labels[i.ID]),
			BlockedBy:  make([]domain.BlockerRef, 0, len(i.BlockedBy)),
			CreatedAt:  isoTime(created(i.CreatedOn, i.ModifiedOn)),
			UpdatedAt:  isoTime(i.ModifiedOn),
		}
		if i.Assignee != nil {
			issue.Assignee = people[*i.Assignee]
		}
		if i.AttachedTo != "" && i.AttachedTo != noParent {
			parent := &domain.ParentRef{ID: i.AttachedTo}
			if len(i.Parents) > 0 && i.Parents[0].ParentID == i.AttachedTo {
				parent.Identifier = i.Parents[0].Identifier
			}
			issue.Parent = parent
		}
		for _, ref := range i.BlockedBy {
			b, ok := blockers[ref.ID]
			if !ok {
				// A deleted or unreadable blocker keeps an unknown state,
				// which dispatch treats as non-terminal.
				issue.BlockedBy = append(issue.BlockedBy, domain.BlockerRef{ID: ref.ID})
				continue
			}
			issue.BlockedBy = append(issue.BlockedBy, domain.BlockerRef{
				ID:         b.ID,
				Identifier: b.Identifier,
				State:      statuses[b.Status].Name,
				PlanKey:    b.PlanKey,
			})
		}
		out = append(out, issue)
	}
	return out, nil
}

// normalizePriority maps Huly's priority, where 0 means no priority and
// 1 (urgent) through 4 (low) order like Sortie's, to the domain priority.
func normalizePriority(p *float64) *int {
	if p == nil {
		return nil
	}
	value := int(*p)
	if value <= 0 || float64(value) != *p {
		return nil
	}
	return &value
}

// personNames returns each person's display name keyed by person id.
func (a *HulyAdapter) personNames(ctx context.Context, ids []string) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}

	raws, err := a.rpc.FindAll(ctx, classPerson, map[string]any{"_id": idIn(ids)}, map[string]any{
		"projection": map[string]any{"_id": 1, "name": 1},
	})
	if err != nil {
		return nil, err
	}
	people, err := decodeAll[hulyPerson](raws)
	if err != nil {
		return nil, err
	}
	for _, p := range people {
		out[p.ID] = displayName(p.Name)
	}
	return out, nil
}

// displayName turns Huly's stored "Last,First" person name into
// "First Last".
func displayName(stored string) string {
	last, first, ok := strings.Cut(stored, ",")
	if !ok {
		return strings.TrimSpace(stored)
	}
	return strings.TrimSpace(strings.TrimSpace(first) + " " + strings.TrimSpace(last))
}

// labelsByIssue returns every label title of each issue, keyed by issue id.
func (a *HulyAdapter) labelsByIssue(ctx context.Context, issueIDs []string) (map[string][]string, error) {
	out := make(map[string][]string, len(issueIDs))
	raws, err := a.rpc.FindAll(ctx, classTagReference, map[string]any{
		"attachedTo": idIn(issueIDs),
		"collection": collectionLabels,
	}, nil)
	if err != nil {
		return nil, err
	}

	type ref struct {
		AttachedTo string `json:"attachedTo"`
		Title      string `json:"title"`
	}
	refs, err := decodeAll[ref](raws)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		out[r.AttachedTo] = append(out[r.AttachedTo], r.Title)
	}
	return out, nil
}

// comments returns the issue's comments in ascending creation order with
// bodies rendered as Markdown.
func (a *HulyAdapter) comments(ctx context.Context, issueID string) ([]domain.Comment, error) {
	raws, err := a.rpc.FindAll(ctx, classChatMessage, map[string]any{
		"attachedTo": issueID,
		"collection": collectionNotes,
	}, map[string]any{"sort": map[string]any{"createdOn": 1}})
	if err != nil {
		return nil, err
	}
	messages, err := decodeAll[hulyMessage](raws)
	if err != nil {
		return nil, err
	}

	var socialIDs []string
	for _, m := range messages {
		socialIDs = append(socialIDs, author(m))
	}
	authors, err := a.authorNames(ctx, dedupe(socialIDs))
	if err != nil {
		return nil, err
	}

	staged := make([]issuekit.SourceComment, 0, len(messages))
	for _, m := range messages {
		staged = append(staged, issuekit.SourceComment{
			ID:        m.ID,
			Author:    authors[author(m)],
			Body:      markup.ToMarkdown(m.Message),
			CreatedAt: isoTime(created(m.CreatedOn, m.ModifiedOn)),
		})
	}
	comments := issuekit.NormalizeComments(staged)
	slices.SortStableFunc(comments, func(x, y domain.Comment) int { return strings.Compare(x.CreatedAt, y.CreatedAt) })
	return comments, nil
}

func author(m hulyMessage) string {
	if m.CreatedBy != "" {
		return m.CreatedBy
	}
	return m.ModifiedBy
}

// authorNames resolves social ids to person display names, falling back to
// the social id's value (an email or login) when no person is attached.
func (a *HulyAdapter) authorNames(ctx context.Context, socialIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(socialIDs))
	if len(socialIDs) == 0 {
		return out, nil
	}

	raws, err := a.rpc.FindAll(ctx, classSocialIdentity, map[string]any{"_id": idIn(socialIDs)}, nil)
	if err != nil {
		return nil, err
	}
	identities, err := decodeAll[hulySocialIdentity](raws)
	if err != nil {
		return nil, err
	}

	var personIDs []string
	for _, id := range identities {
		personIDs = append(personIDs, id.AttachedTo)
	}
	people, err := a.personNames(ctx, dedupe(personIDs))
	if err != nil {
		return nil, err
	}

	for _, id := range identities {
		if name := people[id.AttachedTo]; name != "" {
			out[id.ID] = name
		} else {
			out[id.ID] = id.Value
		}
	}
	return out, nil
}

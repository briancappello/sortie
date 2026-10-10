package huly

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sync"
)

// fakeRPC is an in-memory [rpcClient]. FindAll matches equality and $in
// conditions; Tx applies create, update and remove transactions to the
// store and records them.
type fakeRPC struct {
	mu      sync.Mutex
	docs    map[string][]map[string]any // class -> documents
	markup  map[string]string           // blob source -> serialized markup
	txs     []map[string]any
	txErr   error
	findErr error
}

func newFakeRPC() *fakeRPC {
	return &fakeRPC{docs: map[string][]map[string]any{}, markup: map[string]string{}}
}

func (f *fakeRPC) add(class string, doc map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.docs[class] = append(f.docs[class], doc)
}

func (f *fakeRPC) FindAll(_ context.Context, class string, query map[string]any, options map[string]any) ([]json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.findErr != nil {
		return nil, f.findErr
	}

	limit := -1
	if l, ok := options["limit"].(int); ok {
		limit = l
	}

	out := []json.RawMessage{}
	for _, doc := range f.docs[class] {
		if !matches(doc, query) {
			continue
		}
		raw, err := json.Marshal(doc)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func matches(doc, query map[string]any) bool {
	for field, cond := range query {
		value := fmt.Sprint(doc[field])
		if m, ok := cond.(map[string]any); ok {
			in, _ := m["$in"].([]string)
			found := false
			for _, v := range in {
				if v == value {
					found = true
				}
			}
			if !found {
				return false
			}
			continue
		}
		if fmt.Sprint(cond) != value {
			return false
		}
	}
	return true
}

func (f *fakeRPC) Tx(_ context.Context, tx map[string]any) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.txErr != nil {
		return f.txErr
	}
	f.txs = append(f.txs, tx)

	class, _ := tx["objectClass"].(string)
	id, _ := tx["objectId"].(string)

	switch tx["_class"] {
	case classTxCreateDoc:
		doc := map[string]any{
			"_id": id, "space": tx["objectSpace"], "createdBy": tx["createdBy"],
			"createdOn": tx["modifiedOn"], "modifiedOn": tx["modifiedOn"],
		}
		maps.Copy(doc, tx["attributes"].(map[string]any))
		for _, k := range []string{"attachedTo", "attachedToClass", "collection"} {
			if v, ok := tx[k]; ok {
				doc[k] = v
			}
		}
		f.docs[class] = append(f.docs[class], doc)
	case classTxUpdateDoc:
		for _, doc := range f.docs[class] {
			if doc["_id"] == id {
				maps.Copy(doc, tx["operations"].(map[string]any))
			}
		}
	case classTxRemoveDoc:
		kept := f.docs[class][:0]
		for _, doc := range f.docs[class] {
			if doc["_id"] != id {
				kept = append(kept, doc)
			}
		}
		f.docs[class] = kept
	}
	return nil
}

func (f *fakeRPC) GetMarkup(_ context.Context, _, _, _, source string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.markup[source], nil
}

func (f *fakeRPC) txsOf(class string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []map[string]any
	for _, tx := range f.txs {
		if tx["objectClass"] == class {
			out = append(out, tx)
		}
	}
	return out
}

// seedWorkspace stores a project HULY with the classic Huly workflow, two
// people, and issues HULY-1 (Todo, labeled, blocked by HULY-2), HULY-2
// (Done) and HULY-3 (In Progress, a sub-issue of HULY-1).
func seedWorkspace() *fakeRPC {
	f := newFakeRPC()

	f.add(classProject, map[string]any{"_id": "proj1", "identifier": "HULY", "name": "Huly", "type": "ptype1"})
	f.add(classProjectType, map[string]any{"_id": "ptype1", "statuses": []map[string]any{
		{"_id": "st-backlog"}, {"_id": "st-todo"}, {"_id": "st-progress"}, {"_id": "st-done"}, {"_id": "st-canceled"},
	}})
	for id, name := range map[string]string{
		"st-backlog": "Backlog", "st-todo": "Todo", "st-progress": "In Progress", "st-done": "Done", "st-canceled": "Canceled",
	} {
		f.add(classIssueStatus, map[string]any{"_id": id, "name": name})
	}

	f.add(classPerson, map[string]any{"_id": "person-ada", "name": "Lovelace,Ada"})
	f.add(classPerson, map[string]any{"_id": "person-bot", "name": "Sortie"})
	f.add(classSocialIdentity, map[string]any{"_id": "social-ada", "attachedTo": "person-ada", "value": "ada@example.com"})

	f.add(classIssue, map[string]any{
		"_id": "issue1", "space": "proj1", "identifier": "HULY-1", "title": "First", "status": "st-todo",
		"priority": 2, "assignee": "person-ada", "attachedTo": noParent, "description": "blob1",
		"blockedBy":  []map[string]any{{"_id": "issue2", "_class": classIssue}},
		"createdOn":  1700000000000,
		"modifiedOn": 1700000100000,
	})
	f.add(classIssue, map[string]any{
		"_id": "issue2", "space": "proj1", "identifier": "HULY-2", "title": "Second", "status": "st-done",
		"priority": 0, "attachedTo": noParent, "modifiedOn": 1700000000000,
	})
	f.add(classIssue, map[string]any{
		"_id": "issue3", "space": "proj1", "identifier": "HULY-3", "title": "Child", "status": "st-progress",
		"attachedTo": "issue1", "parents": []map[string]any{{"parentId": "issue1", "identifier": "HULY-1"}},
		"modifiedOn": 1700000000000,
	})

	f.add(classTagElement, map[string]any{"_id": "tag-bug", "title": "Bug", "targetClass": classIssue, "color": 3})
	f.add(classTagReference, map[string]any{
		"_id": "ref1", "space": "proj1", "attachedTo": "issue1", "collection": collectionLabels, "tag": "tag-bug", "title": "Bug",
	})

	f.markup["blob1"] = `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Fix ","marks":[]},{"type":"text","text":"it","marks":[{"type":"bold"}]}]}]}`
	f.add(classChatMessage, map[string]any{
		"_id": "msg2", "attachedTo": "issue1", "collection": collectionNotes, "createdBy": "social-ada",
		"message":   `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"second"}]}]}`,
		"createdOn": 1700000300000, "modifiedOn": 1700000300000,
	})
	f.add(classChatMessage, map[string]any{
		"_id": "msg1", "attachedTo": "issue1", "collection": collectionNotes, "createdBy": "social-unknown",
		"message":   `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"first"}]}]}`,
		"createdOn": 1700000200000, "modifiedOn": 1700000200000,
	})

	return f
}

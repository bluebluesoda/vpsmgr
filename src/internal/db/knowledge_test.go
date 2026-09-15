package db

import "testing"

func TestKnowledgeCRUD(t *testing.T) {
	d, err := Open(t.TempDir() + "/kb.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if list, err := d.ListKnowledge(); err != nil || len(list) != 0 {
		t.Fatalf("empty list = %v, err=%v", list, err)
	}

	id, err := d.CreateKnowledge("First", "# hello")
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.CreateKnowledge("Second", "body")
	if err != nil {
		t.Fatal(err)
	}

	list, err := d.ListKnowledge()
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, err=%v", list, err)
	}
	// Most recently updated first: Second was created last.
	if list[0].ID != second || list[0].Title != "Second" {
		t.Fatalf("unexpected order: %+v", list)
	}

	k, err := d.GetKnowledge(id)
	if err != nil || k == nil || k.Title != "First" || k.Content != "# hello" {
		t.Fatalf("get = %+v, err=%v", k, err)
	}
	if k.CreatedAt == "" || k.UpdatedAt == "" {
		t.Errorf("timestamps missing: %+v", k)
	}
	if missing, err := d.GetKnowledge(9999); err != nil || missing != nil {
		t.Fatalf("missing id = %v, err=%v", missing, err)
	}

	if err := d.UpdateKnowledge(id, "First v2", "updated body"); err != nil {
		t.Fatal(err)
	}
	k, _ = d.GetKnowledge(id)
	if k.Title != "First v2" || k.Content != "updated body" {
		t.Fatalf("update did not stick: %+v", k)
	}
	// Updating moves it to the top of the list.
	list, _ = d.ListKnowledge()
	if list[0].ID != id {
		t.Fatalf("updated article not first: %+v", list)
	}

	if err := d.DeleteKnowledge(id); err != nil {
		t.Fatal(err)
	}
	if k, _ := d.GetKnowledge(id); k != nil {
		t.Fatal("article survived delete")
	}
	// Deleting a missing article is a no-op.
	if err := d.DeleteKnowledge(id); err != nil {
		t.Fatalf("delete of a missing article: %v", err)
	}
}

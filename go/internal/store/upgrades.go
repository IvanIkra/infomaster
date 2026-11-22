package store

// upgrades are the columns a later stage added to a table an earlier stage created: (table, column, definition).
var upgrades = [][3]string{
	{"news", "hidden", "integer not null default 0"},
	{"messages", "subject", "text"},
	{"deadlines", "sheet_id", "integer"},
	{"files", "kind", "text not null default 'pdf'"},
	{"events", "copy_of", "text"},
	{"files", "reading", "text"},
	{"deadlines", "assumed", "integer not null default 0"},
	{"sheets", "due_assumed", "integer not null default 0"},
	{"sheets", "due_at", "text"},
	{"sheets", "sent_at", "text"},
	{"files", "url", "text"},
	{"files", "tries", "integer not null default 0"},
	{"hidden_deadlines", "reason", "text"},
	{"own_deadlines", "course", "text"},
	{"hidden_deadlines", "kept", "text"},
	{"sheets", "caption", "text"},
}

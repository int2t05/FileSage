package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type migration struct {
	version int
	sql     string
}

var migrations = []migration{
	{
		version: 1,
		sql: `
CREATE TABLE file_index (
	relative_path TEXT PRIMARY KEY CHECK (relative_path <> '' AND substr(relative_path, 1, 1) <> '/'),
	parent_path TEXT NOT NULL,
	name TEXT NOT NULL CHECK (name <> ''),
	kind TEXT NOT NULL CHECK (kind IN ('file', 'directory', 'symlink', 'other')),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	modified_at_ns INTEGER NOT NULL,
	mode INTEGER NOT NULL CHECK (mode >= 0),
	device INTEGER,
	inode INTEGER,
	hard_link_count INTEGER CHECK (hard_link_count IS NULL OR hard_link_count > 0),
	media_type TEXT,
	captured_at_ns INTEGER,
	content_sha256 TEXT CHECK (content_sha256 IS NULL OR length(content_sha256) = 64),
	indexed_at_ns INTEGER NOT NULL
);
CREATE INDEX file_index_parent_name ON file_index(parent_path, name);
CREATE INDEX file_index_kind ON file_index(kind);

CREATE TABLE tasks (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	kind TEXT NOT NULL CHECK (kind <> ''),
	status TEXT NOT NULL CHECK (status IN (
		'queued', 'analyzing', 'awaiting_approval', 'running', 'paused',
		'succeeded', 'partial', 'failed', 'cancelled'
	)),
	request_text TEXT NOT NULL DEFAULT '',
	scope_path TEXT NOT NULL DEFAULT '',
	model_provider TEXT NOT NULL DEFAULT '',
	model_name TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);
CREATE INDEX tasks_status_updated ON tasks(status, updated_at_ns DESC);

CREATE TABLE plans (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE RESTRICT,
	version INTEGER NOT NULL CHECK (version > 0),
	status TEXT NOT NULL CHECK (status IN (
		'draft', 'awaiting_approval', 'approved', 'executing', 'completed',
		'failed', 'invalidated', 'rejected', 'cancelled'
	)),
	scope_path TEXT NOT NULL DEFAULT '',
	model_provider TEXT NOT NULL DEFAULT '',
	model_name TEXT NOT NULL DEFAULT '',
	data_scope_json TEXT NOT NULL CHECK (json_valid(data_scope_json) AND json_type(data_scope_json) = 'object'),
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	UNIQUE(task_id, version)
);
CREATE INDEX plans_task_created ON plans(task_id, created_at_ns DESC);

CREATE TABLE operations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	plan_id TEXT NOT NULL REFERENCES plans(id) ON DELETE RESTRICT,
	ordinal INTEGER NOT NULL CHECK (ordinal >= 0),
	action TEXT NOT NULL CHECK (action <> ''),
	source_path TEXT NOT NULL DEFAULT '',
	target_path TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT '',
	evidence TEXT NOT NULL DEFAULT '',
	source_state_json TEXT NOT NULL CHECK (json_valid(source_state_json) AND json_type(source_state_json) = 'object'),
	target_conflict_policy TEXT NOT NULL CHECK (target_conflict_policy IN ('fail', 'rename', 'overwrite')),
	requires_explicit_confirmation INTEGER NOT NULL DEFAULT 0 CHECK (requires_explicit_confirmation IN (0, 1)),
	approval_state TEXT NOT NULL CHECK (approval_state IN ('pending', 'approved', 'rejected', 'invalidated')),
	approved_by TEXT NOT NULL DEFAULT '',
	approved_at_ns INTEGER,
	execution_status TEXT NOT NULL CHECK (execution_status IN (
		'pending', 'running', 'succeeded', 'failed', 'skipped', 'partial', 'cancelled'
	)),
	result_message TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	CHECK (
		(approval_state = 'approved' AND approved_by <> '' AND approved_at_ns IS NOT NULL)
		OR (approval_state <> 'approved' AND approved_by = '' AND approved_at_ns IS NULL)
	),
	UNIQUE(plan_id, ordinal)
);
CREATE INDEX operations_plan_approval ON operations(plan_id, approval_state, ordinal);
CREATE INDEX operations_execution_status ON operations(execution_status, updated_at_ns DESC);

CREATE TABLE audit_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	operation_id TEXT REFERENCES operations(id) ON DELETE RESTRICT,
	event_type TEXT NOT NULL CHECK (event_type <> ''),
	actor TEXT NOT NULL CHECK (actor <> ''),
	outcome TEXT NOT NULL CHECK (outcome <> ''),
	message TEXT NOT NULL DEFAULT '',
	source_path TEXT NOT NULL DEFAULT '',
	target_path TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL
);
CREATE INDEX audit_events_task_created ON audit_events(task_id, created_at_ns DESC);
CREATE INDEX audit_events_operation_created ON audit_events(operation_id, created_at_ns DESC);
`,
	},
	{
		version: 2,
		sql: `
CREATE TABLE trash_items (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	original_path TEXT NOT NULL CHECK (original_path <> ''),
	trash_path TEXT NOT NULL CHECK (trash_path <> ''),
	mount_root TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL CHECK (kind IN ('file', 'directory', 'symlink')),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	modified_at_ns INTEGER NOT NULL,
	mode INTEGER NOT NULL CHECK (mode >= 0),
	device INTEGER,
	inode INTEGER,
	link_count INTEGER,
	identity_available INTEGER NOT NULL CHECK (identity_available IN (0, 1)),
	symlink INTEGER NOT NULL CHECK (symlink IN (0, 1)),
	link_device INTEGER,
	link_inode INTEGER,
	link_count_for_link INTEGER,
	link_identity_available INTEGER NOT NULL CHECK (link_identity_available IN (0, 1)),
	status TEXT NOT NULL CHECK (status IN ('moving', 'trashed', 'restoring', 'purging', 'needs_attention', 'restored', 'purged')),
	deleted_at_ns INTEGER NOT NULL,
	restore_path TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL DEFAULT ''
);
CREATE INDEX trash_items_active ON trash_items(status, id);
`,
	},
	{
		version: 3,
		sql: `
ALTER TABLE trash_items ADD COLUMN state_seq INTEGER NOT NULL DEFAULT 0 CHECK (state_seq >= 0);
UPDATE trash_items SET state_seq = COALESCE((SELECT MAX(id) FROM audit_events), 0);
CREATE INDEX trash_items_snapshot ON trash_items(state_seq, id);
`,
	},
	{
		version: 4,
		sql: `
ALTER TABLE operations ADD COLUMN target_state_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(target_state_json) AND json_type(target_state_json) = 'object');
`,
	},
	{
		version: 5,
		sql: `
CREATE TABLE file_index_scan_runs (
	generation INTEGER PRIMARY KEY CHECK (generation > 0),
	state TEXT NOT NULL CHECK (state IN ('building', 'complete', 'failed')),
	scanned_count INTEGER NOT NULL DEFAULT 0 CHECK (scanned_count >= 0),
	started_at_ns INTEGER NOT NULL,
	finished_at_ns INTEGER,
	error_message TEXT NOT NULL DEFAULT ''
);

CREATE TABLE file_index_state (
	singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
	active_generation INTEGER NOT NULL DEFAULT 0 CHECK (active_generation >= 0),
	building_generation INTEGER,
	next_generation INTEGER NOT NULL DEFAULT 1 CHECK (next_generation > 0),
	state TEXT NOT NULL CHECK (state IN ('not_started', 'building', 'ready', 'stale', 'failed')),
	scanned_count INTEGER NOT NULL DEFAULT 0 CHECK (scanned_count >= 0),
	started_at_ns INTEGER,
	completed_at_ns INTEGER,
	last_error TEXT NOT NULL DEFAULT '',
	CHECK (building_generation IS NULL OR building_generation > 0),
	CHECK (building_generation IS NULL OR state = 'building')
);
INSERT INTO file_index_state (singleton, state) VALUES (1, 'not_started');

CREATE TABLE file_index_generation_entries (
	generation INTEGER NOT NULL REFERENCES file_index_scan_runs(generation) ON DELETE RESTRICT,
	relative_path TEXT NOT NULL CHECK (relative_path <> '' AND substr(relative_path, 1, 1) <> '/'),
	parent_path TEXT NOT NULL,
	name TEXT NOT NULL CHECK (name <> ''),
	kind TEXT NOT NULL CHECK (kind IN ('file', 'directory', 'symlink', 'other')),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	modified_at_ns INTEGER NOT NULL,
	mode INTEGER NOT NULL CHECK (mode >= 0),
	device INTEGER,
	inode INTEGER,
	hard_link_count INTEGER CHECK (hard_link_count IS NULL OR hard_link_count > 0),
	media_type TEXT,
	captured_at_ns INTEGER,
	content_sha256 TEXT CHECK (content_sha256 IS NULL OR length(content_sha256) = 64),
	indexed_at_ns INTEGER NOT NULL,
	PRIMARY KEY (generation, relative_path)
);
CREATE INDEX file_index_generation_parent_name ON file_index_generation_entries(generation, parent_path, name);
CREATE INDEX file_index_generation_kind ON file_index_generation_entries(generation, kind);
CREATE INDEX file_index_generation_size ON file_index_generation_entries(generation, size_bytes);
CREATE INDEX file_index_generation_modified ON file_index_generation_entries(generation, modified_at_ns);
`,
	},
	{
		version: 6,
		// 旧索引未扫描拍摄元数据，需等待新扫描后再开放筛选。
		sql: `
ALTER TABLE file_index_scan_runs
	ADD COLUMN capture_metadata_ready INTEGER NOT NULL DEFAULT 0
	CHECK (capture_metadata_ready IN (0, 1));
`,
	},
	{
		version: 7,
		sql: `
CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
`,
	},
	{
		version: 8,
		sql: `
ALTER TABLE agent_conversations
	ADD COLUMN analysis_action TEXT NOT NULL DEFAULT 'rename'
	CHECK (analysis_action IN ('rename', 'create_directory'));
ALTER TABLE agent_conversations ADD COLUMN target_path TEXT NOT NULL DEFAULT '';
`,
	},
	{
		version: 9,
		// 重建会话表以扩展受支持的分析动作，并保留现有对话与消息。
		sql: `
CREATE TEMP TABLE agent_conversations_v8 AS SELECT * FROM agent_conversations;
CREATE TEMP TABLE agent_conversation_messages_v8 AS SELECT * FROM agent_conversation_messages;
DROP TABLE agent_conversation_messages;
DROP TABLE agent_conversations;

CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	analysis_action TEXT NOT NULL DEFAULT 'rename'
		CHECK (analysis_action IN ('rename', 'create_directory', 'move_file')),
	target_path TEXT NOT NULL DEFAULT '',
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
INSERT INTO agent_conversations (
	id, state, goal, file_path, provider, model, destination, task_id, plan_id,
	error_code, created_at_ns, updated_at_ns, analysis_action, target_path
)
SELECT id, state, goal, file_path, provider, model, destination, task_id, plan_id,
	error_code, created_at_ns, updated_at_ns, analysis_action, target_path
FROM agent_conversations_v8;
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
INSERT INTO agent_conversation_messages (conversation_id, sequence, role, content, created_at_ns)
SELECT conversation_id, sequence, role, content, created_at_ns FROM agent_conversation_messages_v8;
DROP TABLE agent_conversations_v8;
DROP TABLE agent_conversation_messages_v8;
`,
	},
	{
		version: 10,
		// 元数据操作将批准值单独保存；会话表扩展为可恢复的目标确认卡。
		sql: `
ALTER TABLE operations ADD COLUMN mode INTEGER CHECK (mode IS NULL OR mode BETWEEN 0 AND 511);
ALTER TABLE operations ADD COLUMN owner_uid INTEGER CHECK (owner_uid IS NULL OR owner_uid BETWEEN 0 AND 4294967295);
ALTER TABLE operations ADD COLUMN group_gid INTEGER CHECK (group_gid IS NULL OR group_gid BETWEEN 0 AND 4294967295);

CREATE TEMP TABLE agent_conversations_v9 AS SELECT * FROM agent_conversations;
CREATE TEMP TABLE agent_conversation_messages_v9 AS SELECT * FROM agent_conversation_messages;
DROP TABLE agent_conversation_messages;
DROP TABLE agent_conversations;

CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	analysis_action TEXT NOT NULL DEFAULT 'rename'
		CHECK (analysis_action IN ('rename', 'create_directory', 'move_file', 'set_permissions', 'set_owner')),
	target_path TEXT NOT NULL DEFAULT '',
	mode INTEGER CHECK (mode IS NULL OR mode BETWEEN 0 AND 511),
	owner_uid INTEGER CHECK (owner_uid IS NULL OR owner_uid BETWEEN 0 AND 4294967295),
	group_gid INTEGER CHECK (group_gid IS NULL OR group_gid BETWEEN 0 AND 4294967295),
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
INSERT INTO agent_conversations (
	id, state, goal, file_path, provider, model, destination, task_id, plan_id,
	error_code, created_at_ns, updated_at_ns, analysis_action, target_path
)
SELECT id, state, goal, file_path, provider, model, destination, task_id, plan_id,
	error_code, created_at_ns, updated_at_ns, analysis_action, target_path
FROM agent_conversations_v9;
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
INSERT INTO agent_conversation_messages (conversation_id, sequence, role, content, created_at_ns)
SELECT conversation_id, sequence, role, content, created_at_ns FROM agent_conversation_messages_v9;
DROP TABLE agent_conversations_v9;
DROP TABLE agent_conversation_messages_v9;
`,
	},
	{
		version: 11,
		// 保存多项待确认操作，旧单项字段继续供历史数据读取。
		sql: `
ALTER TABLE agent_conversations ADD COLUMN operations_json TEXT NOT NULL DEFAULT '[]'
	CHECK (json_valid(operations_json) AND json_type(operations_json) = 'array');
`,
	},
	{
		version: 12,
		// 扩展会话动作约束，支持文件与文件夹复制。
		sql: `
CREATE TEMP TABLE agent_conversations_v11 AS SELECT * FROM agent_conversations;
CREATE TEMP TABLE agent_conversation_messages_v11 AS SELECT * FROM agent_conversation_messages;
DROP TABLE agent_conversation_messages;
DROP TABLE agent_conversations;

CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	analysis_action TEXT NOT NULL DEFAULT 'rename'
		CHECK (analysis_action IN ('rename', 'create_directory', 'move_file', 'copy_file', 'copy_directory', 'set_permissions', 'set_owner')),
	target_path TEXT NOT NULL DEFAULT '',
	mode INTEGER CHECK (mode IS NULL OR mode BETWEEN 0 AND 511),
	owner_uid INTEGER CHECK (owner_uid IS NULL OR owner_uid BETWEEN 0 AND 4294967295),
	group_gid INTEGER CHECK (group_gid IS NULL OR group_gid BETWEEN 0 AND 4294967295),
	operations_json TEXT NOT NULL DEFAULT '[]'
		CHECK (json_valid(operations_json) AND json_type(operations_json) = 'array'),
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
INSERT INTO agent_conversations (
	id, state, goal, file_path, provider, model, destination, task_id, plan_id,
	error_code, created_at_ns, updated_at_ns, analysis_action, target_path,
	mode, owner_uid, group_gid, operations_json
)
SELECT id, state, goal, file_path, provider, model, destination, task_id, plan_id,
	error_code, created_at_ns, updated_at_ns, analysis_action, target_path,
	mode, owner_uid, group_gid, operations_json
FROM agent_conversations_v11;
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
INSERT INTO agent_conversation_messages (conversation_id, sequence, role, content, created_at_ns)
SELECT conversation_id, sequence, role, content, created_at_ns FROM agent_conversation_messages_v11;
DROP TABLE agent_conversations_v11;
DROP TABLE agent_conversation_messages_v11;
`,
	},
	{
		version: 13,
		// 支持重复扫描会话与结果；扫描失败时结果表保持为空。
		sql: `
CREATE TEMP TABLE agent_conversations_v12 AS SELECT * FROM agent_conversations;
CREATE TEMP TABLE agent_conversation_messages_v12 AS SELECT * FROM agent_conversation_messages;
DROP TABLE agent_conversation_messages;
DROP TABLE agent_conversations;

CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'analysis_complete', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	analysis_action TEXT NOT NULL DEFAULT 'rename'
		CHECK (analysis_action IN ('rename', 'create_directory', 'move_file', 'copy_file', 'copy_directory', 'set_permissions', 'set_owner', 'find_duplicates')),
	target_path TEXT NOT NULL DEFAULT '',
	mode INTEGER CHECK (mode IS NULL OR mode BETWEEN 0 AND 511),
	owner_uid INTEGER CHECK (owner_uid IS NULL OR owner_uid BETWEEN 0 AND 4294967295),
	group_gid INTEGER CHECK (group_gid IS NULL OR group_gid BETWEEN 0 AND 4294967295),
	operations_json TEXT NOT NULL DEFAULT '[]'
		CHECK (json_valid(operations_json) AND json_type(operations_json) = 'array'),
	duplicate_scan_id TEXT,
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
INSERT INTO agent_conversations (
	id, state, goal, file_path, provider, model, destination, task_id, plan_id,
	error_code, created_at_ns, updated_at_ns, analysis_action, target_path,
	mode, owner_uid, group_gid, operations_json
)
SELECT id, state, goal, file_path, provider, model, destination, task_id, plan_id,
	error_code, created_at_ns, updated_at_ns, analysis_action, target_path,
	mode, owner_uid, group_gid, operations_json
FROM agent_conversations_v12;
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
INSERT INTO agent_conversation_messages (conversation_id, sequence, role, content, created_at_ns)
SELECT conversation_id, sequence, role, content, created_at_ns FROM agent_conversation_messages_v12;
DROP TABLE agent_conversations_v12;
DROP TABLE agent_conversation_messages_v12;

CREATE TABLE duplicate_scan_runs (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE RESTRICT,
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	scope_path TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
	phase TEXT NOT NULL CHECK (phase IN ('enumerating', 'hashing', 'complete')),
	entries_visited INTEGER NOT NULL DEFAULT 0 CHECK (entries_visited >= 0),
	candidate_files INTEGER NOT NULL DEFAULT 0 CHECK (candidate_files >= 0),
	files_hashed INTEGER NOT NULL DEFAULT 0 CHECK (files_hashed >= 0),
	bytes_hashed INTEGER NOT NULL DEFAULT 0 CHECK (bytes_hashed >= 0),
	total_bytes INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
	group_count INTEGER NOT NULL DEFAULT 0 CHECK (group_count >= 0),
	duplicate_file_count INTEGER NOT NULL DEFAULT 0 CHECK (duplicate_file_count >= 0),
	reclaimable_bytes INTEGER NOT NULL DEFAULT 0 CHECK (reclaimable_bytes >= 0),
	error_code TEXT NOT NULL DEFAULT '',
	cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);
CREATE INDEX duplicate_scan_runs_conversation ON duplicate_scan_runs(conversation_id, created_at_ns DESC);
CREATE TABLE duplicate_scan_groups (
	scan_id TEXT NOT NULL REFERENCES duplicate_scan_runs(id) ON DELETE CASCADE,
	group_id INTEGER NOT NULL CHECK (group_id > 0),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	duplicate_count INTEGER NOT NULL CHECK (duplicate_count >= 2),
	PRIMARY KEY (scan_id, group_id)
);
CREATE TABLE duplicate_scan_files (
	scan_id TEXT NOT NULL,
	group_id INTEGER NOT NULL,
	path TEXT NOT NULL CHECK (path <> '' AND substr(path, 1, 1) <> '/'),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	modified_at_ns INTEGER NOT NULL,
	PRIMARY KEY (scan_id, group_id, path),
	FOREIGN KEY (scan_id, group_id) REFERENCES duplicate_scan_groups(scan_id, group_id) ON DELETE CASCADE
);
CREATE INDEX duplicate_scan_files_path ON duplicate_scan_files(scan_id, group_id, path);
`,
	},
	{
		version: 14,
		sql: `
ALTER TABLE operations ADD COLUMN proposed_content_sha256 TEXT NOT NULL DEFAULT ''
	CHECK (proposed_content_sha256 = '' OR length(proposed_content_sha256) = 64);
ALTER TABLE operations ADD COLUMN proposed_content_size INTEGER NOT NULL DEFAULT 0
	CHECK (proposed_content_size >= 0);
`,
	},
	{
		version: 15,
		// 扩展分析动作约束，迁移时保留会话与重复扫描历史。
		sql: `
CREATE TEMP TABLE agent_conversations_v14 AS SELECT * FROM agent_conversations;
CREATE TEMP TABLE agent_conversation_messages_v14 AS SELECT * FROM agent_conversation_messages;
CREATE TEMP TABLE duplicate_scan_runs_v14 AS SELECT * FROM duplicate_scan_runs;
CREATE TEMP TABLE duplicate_scan_groups_v14 AS SELECT * FROM duplicate_scan_groups;
CREATE TEMP TABLE duplicate_scan_files_v14 AS SELECT * FROM duplicate_scan_files;

DROP TABLE duplicate_scan_files;
DROP TABLE duplicate_scan_groups;
DROP TABLE duplicate_scan_runs;
DROP TABLE agent_conversation_messages;
DROP TABLE agent_conversations;

CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'analysis_complete', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	analysis_action TEXT NOT NULL DEFAULT 'rename'
		CHECK (analysis_action IN ('rename', 'edit_text', 'create_directory', 'move_file', 'copy_file', 'copy_directory', 'set_permissions', 'set_owner', 'find_duplicates')),
	target_path TEXT NOT NULL DEFAULT '',
	mode INTEGER CHECK (mode IS NULL OR mode BETWEEN 0 AND 511),
	owner_uid INTEGER CHECK (owner_uid IS NULL OR owner_uid BETWEEN 0 AND 4294967295),
	group_gid INTEGER CHECK (group_gid IS NULL OR group_gid BETWEEN 0 AND 4294967295),
	operations_json TEXT NOT NULL DEFAULT '[]'
		CHECK (json_valid(operations_json) AND json_type(operations_json) = 'array'),
	duplicate_scan_id TEXT,
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
INSERT INTO agent_conversations (
	id, state, goal, file_path, provider, model, destination, task_id, plan_id,
	error_code, created_at_ns, updated_at_ns, analysis_action, target_path,
	mode, owner_uid, group_gid, operations_json, duplicate_scan_id
)
SELECT id, state, goal, file_path, provider, model, destination, task_id, plan_id,
	error_code, created_at_ns, updated_at_ns, analysis_action, target_path,
	mode, owner_uid, group_gid, operations_json, duplicate_scan_id
FROM agent_conversations_v14;
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
INSERT INTO agent_conversation_messages (conversation_id, sequence, role, content, created_at_ns)
SELECT conversation_id, sequence, role, content, created_at_ns FROM agent_conversation_messages_v14;

CREATE TABLE duplicate_scan_runs (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE RESTRICT,
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	scope_path TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
	phase TEXT NOT NULL CHECK (phase IN ('enumerating', 'hashing', 'complete')),
	entries_visited INTEGER NOT NULL DEFAULT 0 CHECK (entries_visited >= 0),
	candidate_files INTEGER NOT NULL DEFAULT 0 CHECK (candidate_files >= 0),
	files_hashed INTEGER NOT NULL DEFAULT 0 CHECK (files_hashed >= 0),
	bytes_hashed INTEGER NOT NULL DEFAULT 0 CHECK (bytes_hashed >= 0),
	total_bytes INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
	group_count INTEGER NOT NULL DEFAULT 0 CHECK (group_count >= 0),
	duplicate_file_count INTEGER NOT NULL DEFAULT 0 CHECK (duplicate_file_count >= 0),
	reclaimable_bytes INTEGER NOT NULL DEFAULT 0 CHECK (reclaimable_bytes >= 0),
	error_code TEXT NOT NULL DEFAULT '',
	cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);
INSERT INTO duplicate_scan_runs SELECT * FROM duplicate_scan_runs_v14;
CREATE INDEX duplicate_scan_runs_conversation ON duplicate_scan_runs(conversation_id, created_at_ns DESC);

CREATE TABLE duplicate_scan_groups (
	scan_id TEXT NOT NULL REFERENCES duplicate_scan_runs(id) ON DELETE CASCADE,
	group_id INTEGER NOT NULL CHECK (group_id > 0),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	duplicate_count INTEGER NOT NULL CHECK (duplicate_count >= 2),
	PRIMARY KEY (scan_id, group_id)
);
INSERT INTO duplicate_scan_groups SELECT * FROM duplicate_scan_groups_v14;

CREATE TABLE duplicate_scan_files (
	scan_id TEXT NOT NULL,
	group_id INTEGER NOT NULL,
	path TEXT NOT NULL CHECK (path <> '' AND substr(path, 1, 1) <> '/'),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	modified_at_ns INTEGER NOT NULL,
	PRIMARY KEY (scan_id, group_id, path),
	FOREIGN KEY (scan_id, group_id) REFERENCES duplicate_scan_groups(scan_id, group_id) ON DELETE CASCADE
);
INSERT INTO duplicate_scan_files SELECT * FROM duplicate_scan_files_v14;
CREATE INDEX duplicate_scan_files_path ON duplicate_scan_files(scan_id, group_id, path);

DROP TABLE agent_conversations_v14;
DROP TABLE agent_conversation_messages_v14;
DROP TABLE duplicate_scan_runs_v14;
DROP TABLE duplicate_scan_groups_v14;
DROP TABLE duplicate_scan_files_v14;
`,
	},
	{
		version: 16,
		// 扩展分析动作约束，迁移时保留会话与重复扫描历史。
		sql: `
CREATE TEMP TABLE agent_conversations_v15 AS SELECT * FROM agent_conversations;
CREATE TEMP TABLE agent_conversation_messages_v15 AS SELECT * FROM agent_conversation_messages;
CREATE TEMP TABLE duplicate_scan_runs_v15 AS SELECT * FROM duplicate_scan_runs;
CREATE TEMP TABLE duplicate_scan_groups_v15 AS SELECT * FROM duplicate_scan_groups;
CREATE TEMP TABLE duplicate_scan_files_v15 AS SELECT * FROM duplicate_scan_files;

DROP TABLE duplicate_scan_files;
DROP TABLE duplicate_scan_groups;
DROP TABLE duplicate_scan_runs;
DROP TABLE agent_conversation_messages;
DROP TABLE agent_conversations;

CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'analysis_complete', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	analysis_action TEXT NOT NULL DEFAULT 'rename'
		CHECK (analysis_action IN ('rename', 'edit_text', 'create_directory', 'move_file', 'copy_file', 'copy_directory', 'set_permissions', 'set_owner', 'find_duplicates', 'archive_photos')),
	target_path TEXT NOT NULL DEFAULT '',
	mode INTEGER CHECK (mode IS NULL OR mode BETWEEN 0 AND 511),
	owner_uid INTEGER CHECK (owner_uid IS NULL OR owner_uid BETWEEN 0 AND 4294967295),
	group_gid INTEGER CHECK (group_gid IS NULL OR group_gid BETWEEN 0 AND 4294967295),
	operations_json TEXT NOT NULL DEFAULT '[]'
		CHECK (json_valid(operations_json) AND json_type(operations_json) = 'array'),
	duplicate_scan_id TEXT,
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
INSERT INTO agent_conversations (
	id, state, goal, file_path, provider, model, destination, task_id, plan_id,
	error_code, created_at_ns, updated_at_ns, analysis_action, target_path,
	mode, owner_uid, group_gid, operations_json, duplicate_scan_id
)
SELECT id, state, goal, file_path, provider, model, destination, task_id, plan_id,
	error_code, created_at_ns, updated_at_ns, analysis_action, target_path,
	mode, owner_uid, group_gid, operations_json, duplicate_scan_id
FROM agent_conversations_v15;
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
INSERT INTO agent_conversation_messages (conversation_id, sequence, role, content, created_at_ns)
SELECT conversation_id, sequence, role, content, created_at_ns FROM agent_conversation_messages_v15;

CREATE TABLE duplicate_scan_runs (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE RESTRICT,
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	scope_path TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
	phase TEXT NOT NULL CHECK (phase IN ('enumerating', 'hashing', 'complete')),
	entries_visited INTEGER NOT NULL DEFAULT 0 CHECK (entries_visited >= 0),
	candidate_files INTEGER NOT NULL DEFAULT 0 CHECK (candidate_files >= 0),
	files_hashed INTEGER NOT NULL DEFAULT 0 CHECK (files_hashed >= 0),
	bytes_hashed INTEGER NOT NULL DEFAULT 0 CHECK (bytes_hashed >= 0),
	total_bytes INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
	group_count INTEGER NOT NULL DEFAULT 0 CHECK (group_count >= 0),
	duplicate_file_count INTEGER NOT NULL DEFAULT 0 CHECK (duplicate_file_count >= 0),
	reclaimable_bytes INTEGER NOT NULL DEFAULT 0 CHECK (reclaimable_bytes >= 0),
	error_code TEXT NOT NULL DEFAULT '',
	cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);
INSERT INTO duplicate_scan_runs SELECT * FROM duplicate_scan_runs_v15;
CREATE INDEX duplicate_scan_runs_conversation ON duplicate_scan_runs(conversation_id, created_at_ns DESC);

CREATE TABLE duplicate_scan_groups (
	scan_id TEXT NOT NULL REFERENCES duplicate_scan_runs(id) ON DELETE CASCADE,
	group_id INTEGER NOT NULL CHECK (group_id > 0),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	duplicate_count INTEGER NOT NULL CHECK (duplicate_count >= 2),
	PRIMARY KEY (scan_id, group_id)
);
INSERT INTO duplicate_scan_groups SELECT * FROM duplicate_scan_groups_v15;

CREATE TABLE duplicate_scan_files (
	scan_id TEXT NOT NULL,
	group_id INTEGER NOT NULL,
	path TEXT NOT NULL CHECK (path <> '' AND substr(path, 1, 1) <> '/'),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	modified_at_ns INTEGER NOT NULL,
	PRIMARY KEY (scan_id, group_id, path),
	FOREIGN KEY (scan_id, group_id) REFERENCES duplicate_scan_groups(scan_id, group_id) ON DELETE CASCADE
);
INSERT INTO duplicate_scan_files SELECT * FROM duplicate_scan_files_v15;
CREATE INDEX duplicate_scan_files_path ON duplicate_scan_files(scan_id, group_id, path);

DROP TABLE agent_conversations_v15;
DROP TABLE agent_conversation_messages_v15;
DROP TABLE duplicate_scan_runs_v15;
DROP TABLE duplicate_scan_groups_v15;
DROP TABLE duplicate_scan_files_v15;
`,
	},
	{
		version: 17,
		// 加入分类确认卡动作，迁移时保留对话、重复扫描和文件索引历史。
		sql: `
CREATE TEMP TABLE agent_conversations_v16 AS SELECT * FROM agent_conversations;
CREATE TEMP TABLE agent_conversation_messages_v16 AS SELECT * FROM agent_conversation_messages;
CREATE TEMP TABLE duplicate_scan_runs_v16 AS SELECT * FROM duplicate_scan_runs;
CREATE TEMP TABLE duplicate_scan_groups_v16 AS SELECT * FROM duplicate_scan_groups;
CREATE TEMP TABLE duplicate_scan_files_v16 AS SELECT * FROM duplicate_scan_files;

DROP TABLE duplicate_scan_files;
DROP TABLE duplicate_scan_groups;
DROP TABLE duplicate_scan_runs;
DROP TABLE agent_conversation_messages;
DROP TABLE agent_conversations;

CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'analysis_complete', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	analysis_action TEXT NOT NULL DEFAULT 'rename'
		CHECK (analysis_action IN ('rename', 'edit_text', 'create_directory', 'move_file', 'copy_file', 'copy_directory', 'set_permissions', 'set_owner', 'find_duplicates', 'archive_photos', 'classify_documents')),
	target_path TEXT NOT NULL DEFAULT '',
	mode INTEGER CHECK (mode IS NULL OR mode BETWEEN 0 AND 511),
	owner_uid INTEGER CHECK (owner_uid IS NULL OR owner_uid BETWEEN 0 AND 4294967295),
	group_gid INTEGER CHECK (group_gid IS NULL OR group_gid BETWEEN 0 AND 4294967295),
	operations_json TEXT NOT NULL DEFAULT '[]'
		CHECK (json_valid(operations_json) AND json_type(operations_json) = 'array'),
	duplicate_scan_id TEXT,
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
INSERT INTO agent_conversations SELECT * FROM agent_conversations_v16;
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
INSERT INTO agent_conversation_messages SELECT * FROM agent_conversation_messages_v16;

CREATE TABLE duplicate_scan_runs (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE RESTRICT,
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	scope_path TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
	phase TEXT NOT NULL CHECK (phase IN ('enumerating', 'hashing', 'complete')),
	entries_visited INTEGER NOT NULL DEFAULT 0 CHECK (entries_visited >= 0),
	candidate_files INTEGER NOT NULL DEFAULT 0 CHECK (candidate_files >= 0),
	files_hashed INTEGER NOT NULL DEFAULT 0 CHECK (files_hashed >= 0),
	bytes_hashed INTEGER NOT NULL DEFAULT 0 CHECK (bytes_hashed >= 0),
	total_bytes INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
	group_count INTEGER NOT NULL DEFAULT 0 CHECK (group_count >= 0),
	duplicate_file_count INTEGER NOT NULL DEFAULT 0 CHECK (duplicate_file_count >= 0),
	reclaimable_bytes INTEGER NOT NULL DEFAULT 0 CHECK (reclaimable_bytes >= 0),
	error_code TEXT NOT NULL DEFAULT '',
	cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);
INSERT INTO duplicate_scan_runs SELECT * FROM duplicate_scan_runs_v16;
CREATE INDEX duplicate_scan_runs_conversation ON duplicate_scan_runs(conversation_id, created_at_ns DESC);

CREATE TABLE duplicate_scan_groups (
	scan_id TEXT NOT NULL REFERENCES duplicate_scan_runs(id) ON DELETE CASCADE,
	group_id INTEGER NOT NULL CHECK (group_id > 0),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	duplicate_count INTEGER NOT NULL CHECK (duplicate_count >= 2),
	PRIMARY KEY (scan_id, group_id)
);
INSERT INTO duplicate_scan_groups SELECT * FROM duplicate_scan_groups_v16;

CREATE TABLE duplicate_scan_files (
	scan_id TEXT NOT NULL,
	group_id INTEGER NOT NULL,
	path TEXT NOT NULL CHECK (path <> '' AND substr(path, 1, 1) <> '/'),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	modified_at_ns INTEGER NOT NULL,
	PRIMARY KEY (scan_id, group_id, path),
	FOREIGN KEY (scan_id, group_id) REFERENCES duplicate_scan_groups(scan_id, group_id) ON DELETE CASCADE
);
INSERT INTO duplicate_scan_files SELECT * FROM duplicate_scan_files_v16;
CREATE INDEX duplicate_scan_files_path ON duplicate_scan_files(scan_id, group_id, path);

DROP TABLE agent_conversations_v16;
DROP TABLE agent_conversation_messages_v16;
DROP TABLE duplicate_scan_runs_v16;
DROP TABLE duplicate_scan_groups_v16;
DROP TABLE duplicate_scan_files_v16;
`,
	},
	{
		version: 18,
		// 加入软链接确认卡动作，并保留对话与重复扫描历史。
		sql: `
CREATE TEMP TABLE agent_conversations_v17 AS SELECT * FROM agent_conversations;
CREATE TEMP TABLE agent_conversation_messages_v17 AS SELECT * FROM agent_conversation_messages;
CREATE TEMP TABLE duplicate_scan_runs_v17 AS SELECT * FROM duplicate_scan_runs;
CREATE TEMP TABLE duplicate_scan_groups_v17 AS SELECT * FROM duplicate_scan_groups;
CREATE TEMP TABLE duplicate_scan_files_v17 AS SELECT * FROM duplicate_scan_files;

DROP TABLE duplicate_scan_files;
DROP TABLE duplicate_scan_groups;
DROP TABLE duplicate_scan_runs;
DROP TABLE agent_conversation_messages;
DROP TABLE agent_conversations;

CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'analysis_complete', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	analysis_action TEXT NOT NULL DEFAULT 'rename'
		CHECK (analysis_action IN ('rename', 'edit_text', 'create_directory', 'move_file', 'create_symlink', 'copy_file', 'copy_directory', 'set_permissions', 'set_owner', 'find_duplicates', 'archive_photos', 'classify_documents')),
	target_path TEXT NOT NULL DEFAULT '',
	mode INTEGER CHECK (mode IS NULL OR mode BETWEEN 0 AND 511),
	owner_uid INTEGER CHECK (owner_uid IS NULL OR owner_uid BETWEEN 0 AND 4294967295),
	group_gid INTEGER CHECK (group_gid IS NULL OR group_gid BETWEEN 0 AND 4294967295),
	operations_json TEXT NOT NULL DEFAULT '[]'
		CHECK (json_valid(operations_json) AND json_type(operations_json) = 'array'),
	duplicate_scan_id TEXT,
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
INSERT INTO agent_conversations SELECT * FROM agent_conversations_v17;
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
INSERT INTO agent_conversation_messages SELECT * FROM agent_conversation_messages_v17;

CREATE TABLE duplicate_scan_runs (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE RESTRICT,
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	scope_path TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
	phase TEXT NOT NULL CHECK (phase IN ('enumerating', 'hashing', 'complete')),
	entries_visited INTEGER NOT NULL DEFAULT 0 CHECK (entries_visited >= 0),
	candidate_files INTEGER NOT NULL DEFAULT 0 CHECK (candidate_files >= 0),
	files_hashed INTEGER NOT NULL DEFAULT 0 CHECK (files_hashed >= 0),
	bytes_hashed INTEGER NOT NULL DEFAULT 0 CHECK (bytes_hashed >= 0),
	total_bytes INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
	group_count INTEGER NOT NULL DEFAULT 0 CHECK (group_count >= 0),
	duplicate_file_count INTEGER NOT NULL DEFAULT 0 CHECK (duplicate_file_count >= 0),
	reclaimable_bytes INTEGER NOT NULL DEFAULT 0 CHECK (reclaimable_bytes >= 0),
	error_code TEXT NOT NULL DEFAULT '',
	cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);
INSERT INTO duplicate_scan_runs SELECT * FROM duplicate_scan_runs_v17;
CREATE INDEX duplicate_scan_runs_conversation ON duplicate_scan_runs(conversation_id, created_at_ns DESC);

CREATE TABLE duplicate_scan_groups (
	scan_id TEXT NOT NULL REFERENCES duplicate_scan_runs(id) ON DELETE CASCADE,
	group_id INTEGER NOT NULL CHECK (group_id > 0),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	duplicate_count INTEGER NOT NULL CHECK (duplicate_count >= 2),
	PRIMARY KEY (scan_id, group_id)
);
INSERT INTO duplicate_scan_groups SELECT * FROM duplicate_scan_groups_v17;

CREATE TABLE duplicate_scan_files (
	scan_id TEXT NOT NULL,
	group_id INTEGER NOT NULL,
	path TEXT NOT NULL CHECK (path <> '' AND substr(path, 1, 1) <> '/'),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	modified_at_ns INTEGER NOT NULL,
	PRIMARY KEY (scan_id, group_id, path),
	FOREIGN KEY (scan_id, group_id) REFERENCES duplicate_scan_groups(scan_id, group_id) ON DELETE CASCADE
);
INSERT INTO duplicate_scan_files SELECT * FROM duplicate_scan_files_v17;
CREATE INDEX duplicate_scan_files_path ON duplicate_scan_files(scan_id, group_id, path);

DROP TABLE agent_conversations_v17;
DROP TABLE agent_conversation_messages_v17;
DROP TABLE duplicate_scan_runs_v17;
DROP TABLE duplicate_scan_groups_v17;
DROP TABLE duplicate_scan_files_v17;
`,
	},
	{
		version: 19,
		// 持久化 Agent 方案停止请求，确保重启后不会继续后续操作。
		sql: `
ALTER TABLE plans ADD COLUMN stop_requested INTEGER NOT NULL DEFAULT 0
	CHECK (stop_requested IN (0, 1));
`,
	},
	{
		version: 20,
		// 扩展对话动作约束，并保留已有会话、消息与重复扫描结果。
		sql: `
CREATE TEMP TABLE agent_conversations_v19 AS SELECT * FROM agent_conversations;
CREATE TEMP TABLE agent_conversation_messages_v19 AS SELECT * FROM agent_conversation_messages;
CREATE TEMP TABLE duplicate_scan_runs_v19 AS SELECT * FROM duplicate_scan_runs;
CREATE TEMP TABLE duplicate_scan_groups_v19 AS SELECT * FROM duplicate_scan_groups;
CREATE TEMP TABLE duplicate_scan_files_v19 AS SELECT * FROM duplicate_scan_files;

DROP TABLE duplicate_scan_files;
DROP TABLE duplicate_scan_groups;
DROP TABLE duplicate_scan_runs;
DROP TABLE agent_conversation_messages;
DROP TABLE agent_conversations;

CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'analysis_complete', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	analysis_action TEXT NOT NULL DEFAULT 'rename'
		CHECK (analysis_action IN ('rename', 'edit_text', 'create_directory', 'move_file', 'create_symlink', 'copy_file', 'copy_directory', 'set_permissions', 'set_owner', 'find_duplicates', 'archive_photos', 'classify_documents', 'trash')),
	target_path TEXT NOT NULL DEFAULT '',
	mode INTEGER CHECK (mode IS NULL OR mode BETWEEN 0 AND 511),
	owner_uid INTEGER CHECK (owner_uid IS NULL OR owner_uid BETWEEN 0 AND 4294967295),
	group_gid INTEGER CHECK (group_gid IS NULL OR group_gid BETWEEN 0 AND 4294967295),
	operations_json TEXT NOT NULL DEFAULT '[]'
		CHECK (json_valid(operations_json) AND json_type(operations_json) = 'array'),
	duplicate_scan_id TEXT,
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
INSERT INTO agent_conversations SELECT * FROM agent_conversations_v19;
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
INSERT INTO agent_conversation_messages SELECT * FROM agent_conversation_messages_v19;

CREATE TABLE duplicate_scan_runs (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE RESTRICT,
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	scope_path TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
	phase TEXT NOT NULL CHECK (phase IN ('enumerating', 'hashing', 'complete')),
	entries_visited INTEGER NOT NULL DEFAULT 0 CHECK (entries_visited >= 0),
	candidate_files INTEGER NOT NULL DEFAULT 0 CHECK (candidate_files >= 0),
	files_hashed INTEGER NOT NULL DEFAULT 0 CHECK (files_hashed >= 0),
	bytes_hashed INTEGER NOT NULL DEFAULT 0 CHECK (bytes_hashed >= 0),
	total_bytes INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
	group_count INTEGER NOT NULL DEFAULT 0 CHECK (group_count >= 0),
	duplicate_file_count INTEGER NOT NULL DEFAULT 0 CHECK (duplicate_file_count >= 0),
	reclaimable_bytes INTEGER NOT NULL DEFAULT 0 CHECK (reclaimable_bytes >= 0),
	error_code TEXT NOT NULL DEFAULT '',
	cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);
INSERT INTO duplicate_scan_runs SELECT * FROM duplicate_scan_runs_v19;
CREATE INDEX duplicate_scan_runs_conversation ON duplicate_scan_runs(conversation_id, created_at_ns DESC);

CREATE TABLE duplicate_scan_groups (
	scan_id TEXT NOT NULL REFERENCES duplicate_scan_runs(id) ON DELETE CASCADE,
	group_id INTEGER NOT NULL CHECK (group_id > 0),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	duplicate_count INTEGER NOT NULL CHECK (duplicate_count >= 2),
	PRIMARY KEY (scan_id, group_id)
);
INSERT INTO duplicate_scan_groups SELECT * FROM duplicate_scan_groups_v19;

CREATE TABLE duplicate_scan_files (
	scan_id TEXT NOT NULL,
	group_id INTEGER NOT NULL,
	path TEXT NOT NULL CHECK (path <> '' AND substr(path, 1, 1) <> '/'),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	modified_at_ns INTEGER NOT NULL,
	PRIMARY KEY (scan_id, group_id, path),
	FOREIGN KEY (scan_id, group_id) REFERENCES duplicate_scan_groups(scan_id, group_id) ON DELETE CASCADE
);
INSERT INTO duplicate_scan_files SELECT * FROM duplicate_scan_files_v19;
CREATE INDEX duplicate_scan_files_path ON duplicate_scan_files(scan_id, group_id, path);

DROP TABLE agent_conversations_v19;
DROP TABLE agent_conversation_messages_v19;
DROP TABLE duplicate_scan_runs_v19;
DROP TABLE duplicate_scan_groups_v19;
DROP TABLE duplicate_scan_files_v19;
`,
	},
	{
		version: 21,
		// 支持明确审批的永久删除动作，并保留会话、消息与扫描历史。
		sql: `
CREATE TEMP TABLE agent_conversations_v20 AS SELECT * FROM agent_conversations;
CREATE TEMP TABLE agent_conversation_messages_v20 AS SELECT * FROM agent_conversation_messages;
CREATE TEMP TABLE duplicate_scan_runs_v20 AS SELECT * FROM duplicate_scan_runs;
CREATE TEMP TABLE duplicate_scan_groups_v20 AS SELECT * FROM duplicate_scan_groups;
CREATE TEMP TABLE duplicate_scan_files_v20 AS SELECT * FROM duplicate_scan_files;

DROP TABLE duplicate_scan_files;
DROP TABLE duplicate_scan_groups;
DROP TABLE duplicate_scan_runs;
DROP TABLE agent_conversation_messages;
DROP TABLE agent_conversations;

CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'analysis_complete', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	analysis_action TEXT NOT NULL DEFAULT 'rename'
		CHECK (analysis_action IN ('rename', 'edit_text', 'create_directory', 'move_file', 'create_symlink', 'copy_file', 'copy_directory', 'set_permissions', 'set_owner', 'find_duplicates', 'archive_photos', 'classify_documents', 'trash', 'permanent_delete')),
	target_path TEXT NOT NULL DEFAULT '',
	mode INTEGER CHECK (mode IS NULL OR mode BETWEEN 0 AND 511),
	owner_uid INTEGER CHECK (owner_uid IS NULL OR owner_uid BETWEEN 0 AND 4294967295),
	group_gid INTEGER CHECK (group_gid IS NULL OR group_gid BETWEEN 0 AND 4294967295),
	operations_json TEXT NOT NULL DEFAULT '[]'
		CHECK (json_valid(operations_json) AND json_type(operations_json) = 'array'),
	duplicate_scan_id TEXT,
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
INSERT INTO agent_conversations SELECT * FROM agent_conversations_v20;
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
INSERT INTO agent_conversation_messages SELECT * FROM agent_conversation_messages_v20;

CREATE TABLE duplicate_scan_runs (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE RESTRICT,
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	scope_path TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
	phase TEXT NOT NULL CHECK (phase IN ('enumerating', 'hashing', 'complete')),
	entries_visited INTEGER NOT NULL DEFAULT 0 CHECK (entries_visited >= 0),
	candidate_files INTEGER NOT NULL DEFAULT 0 CHECK (candidate_files >= 0),
	files_hashed INTEGER NOT NULL DEFAULT 0 CHECK (files_hashed >= 0),
	bytes_hashed INTEGER NOT NULL DEFAULT 0 CHECK (bytes_hashed >= 0),
	total_bytes INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
	group_count INTEGER NOT NULL DEFAULT 0 CHECK (group_count >= 0),
	duplicate_file_count INTEGER NOT NULL DEFAULT 0 CHECK (duplicate_file_count >= 0),
	reclaimable_bytes INTEGER NOT NULL DEFAULT 0 CHECK (reclaimable_bytes >= 0),
	error_code TEXT NOT NULL DEFAULT '',
	cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);
INSERT INTO duplicate_scan_runs SELECT * FROM duplicate_scan_runs_v20;
CREATE INDEX duplicate_scan_runs_conversation ON duplicate_scan_runs(conversation_id, created_at_ns DESC);

CREATE TABLE duplicate_scan_groups (
	scan_id TEXT NOT NULL REFERENCES duplicate_scan_runs(id) ON DELETE CASCADE,
	group_id INTEGER NOT NULL CHECK (group_id > 0),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	duplicate_count INTEGER NOT NULL CHECK (duplicate_count >= 2),
	PRIMARY KEY (scan_id, group_id)
);
INSERT INTO duplicate_scan_groups SELECT * FROM duplicate_scan_groups_v20;

CREATE TABLE duplicate_scan_files (
	scan_id TEXT NOT NULL,
	group_id INTEGER NOT NULL,
	path TEXT NOT NULL CHECK (path <> '' AND substr(path, 1, 1) <> '/'),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	modified_at_ns INTEGER NOT NULL,
	PRIMARY KEY (scan_id, group_id, path),
	FOREIGN KEY (scan_id, group_id) REFERENCES duplicate_scan_groups(scan_id, group_id) ON DELETE CASCADE
);
INSERT INTO duplicate_scan_files SELECT * FROM duplicate_scan_files_v20;
CREATE INDEX duplicate_scan_files_path ON duplicate_scan_files(scan_id, group_id, path);

DROP TABLE agent_conversations_v20;
DROP TABLE agent_conversation_messages_v20;
DROP TABLE duplicate_scan_runs_v20;
DROP TABLE duplicate_scan_groups_v20;
DROP TABLE duplicate_scan_files_v20;
`,
	},
	{
		version: 22,
		// 增加 Agent 归档动作并保留已有会话与重复扫描历史。
		sql: `
CREATE TEMP TABLE agent_conversations_v21 AS SELECT * FROM agent_conversations;
CREATE TEMP TABLE agent_conversation_messages_v21 AS SELECT * FROM agent_conversation_messages;
CREATE TEMP TABLE duplicate_scan_runs_v21 AS SELECT * FROM duplicate_scan_runs;
CREATE TEMP TABLE duplicate_scan_groups_v21 AS SELECT * FROM duplicate_scan_groups;
CREATE TEMP TABLE duplicate_scan_files_v21 AS SELECT * FROM duplicate_scan_files;

DROP TABLE duplicate_scan_files;
DROP TABLE duplicate_scan_groups;
DROP TABLE duplicate_scan_runs;
DROP TABLE agent_conversation_messages;
DROP TABLE agent_conversations;

CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'analysis_complete', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	analysis_action TEXT NOT NULL DEFAULT 'rename'
		CHECK (analysis_action IN ('rename', 'edit_text', 'create_directory', 'move_file', 'create_symlink', 'copy_file', 'copy_directory', 'create_archive', 'extract_archive', 'set_permissions', 'set_owner', 'find_duplicates', 'archive_photos', 'classify_documents', 'trash', 'permanent_delete')),
	target_path TEXT NOT NULL DEFAULT '',
	mode INTEGER CHECK (mode IS NULL OR mode BETWEEN 0 AND 511),
	owner_uid INTEGER CHECK (owner_uid IS NULL OR owner_uid BETWEEN 0 AND 4294967295),
	group_gid INTEGER CHECK (group_gid IS NULL OR group_gid BETWEEN 0 AND 4294967295),
	operations_json TEXT NOT NULL DEFAULT '[]'
		CHECK (json_valid(operations_json) AND json_type(operations_json) = 'array'),
	duplicate_scan_id TEXT,
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
INSERT INTO agent_conversations SELECT * FROM agent_conversations_v21;
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
INSERT INTO agent_conversation_messages SELECT * FROM agent_conversation_messages_v21;

CREATE TABLE duplicate_scan_runs (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE RESTRICT,
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	scope_path TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
	phase TEXT NOT NULL CHECK (phase IN ('enumerating', 'hashing', 'complete')),
	entries_visited INTEGER NOT NULL DEFAULT 0 CHECK (entries_visited >= 0),
	candidate_files INTEGER NOT NULL DEFAULT 0 CHECK (candidate_files >= 0),
	files_hashed INTEGER NOT NULL DEFAULT 0 CHECK (files_hashed >= 0),
	bytes_hashed INTEGER NOT NULL DEFAULT 0 CHECK (bytes_hashed >= 0),
	total_bytes INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
	group_count INTEGER NOT NULL DEFAULT 0 CHECK (group_count >= 0),
	duplicate_file_count INTEGER NOT NULL DEFAULT 0 CHECK (duplicate_file_count >= 0),
	reclaimable_bytes INTEGER NOT NULL DEFAULT 0 CHECK (reclaimable_bytes >= 0),
	error_code TEXT NOT NULL DEFAULT '',
	cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);
INSERT INTO duplicate_scan_runs SELECT * FROM duplicate_scan_runs_v21;
CREATE INDEX duplicate_scan_runs_conversation ON duplicate_scan_runs(conversation_id, created_at_ns DESC);

CREATE TABLE duplicate_scan_groups (
	scan_id TEXT NOT NULL REFERENCES duplicate_scan_runs(id) ON DELETE CASCADE,
	group_id INTEGER NOT NULL CHECK (group_id > 0),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	duplicate_count INTEGER NOT NULL CHECK (duplicate_count >= 2),
	PRIMARY KEY (scan_id, group_id)
);
INSERT INTO duplicate_scan_groups SELECT * FROM duplicate_scan_groups_v21;

CREATE TABLE duplicate_scan_files (
	scan_id TEXT NOT NULL,
	group_id INTEGER NOT NULL,
	path TEXT NOT NULL CHECK (path <> '' AND substr(path, 1, 1) <> '/'),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	modified_at_ns INTEGER NOT NULL,
	PRIMARY KEY (scan_id, group_id, path),
	FOREIGN KEY (scan_id, group_id) REFERENCES duplicate_scan_groups(scan_id, group_id) ON DELETE CASCADE
);
INSERT INTO duplicate_scan_files SELECT * FROM duplicate_scan_files_v21;
CREATE INDEX duplicate_scan_files_path ON duplicate_scan_files(scan_id, group_id, path);

DROP TABLE agent_conversations_v21;
DROP TABLE agent_conversation_messages_v21;
DROP TABLE duplicate_scan_runs_v21;
DROP TABLE duplicate_scan_groups_v21;
DROP TABLE duplicate_scan_files_v21;
`,
	},
	{
		version: 23,
		// 增加 Shell 确认动作，并保存逐路径审批摘要，不保存文件正文或命令输出。
		sql: `
CREATE TEMP TABLE agent_conversations_v22 AS SELECT * FROM agent_conversations;
CREATE TEMP TABLE agent_conversation_messages_v22 AS SELECT * FROM agent_conversation_messages;
CREATE TEMP TABLE duplicate_scan_runs_v22 AS SELECT * FROM duplicate_scan_runs;
CREATE TEMP TABLE duplicate_scan_groups_v22 AS SELECT * FROM duplicate_scan_groups;
CREATE TEMP TABLE duplicate_scan_files_v22 AS SELECT * FROM duplicate_scan_files;

DROP TABLE duplicate_scan_files;
DROP TABLE duplicate_scan_groups;
DROP TABLE duplicate_scan_runs;
DROP TABLE agent_conversation_messages;
DROP TABLE agent_conversations;

CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'analysis_complete', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	analysis_action TEXT NOT NULL DEFAULT 'rename'
		CHECK (analysis_action IN ('rename', 'edit_text', 'create_directory', 'move_file', 'create_symlink', 'copy_file', 'copy_directory', 'create_archive', 'extract_archive', 'set_permissions', 'set_owner', 'find_duplicates', 'archive_photos', 'classify_documents', 'trash', 'permanent_delete', 'run_shell')),
	target_path TEXT NOT NULL DEFAULT '',
	mode INTEGER CHECK (mode IS NULL OR mode BETWEEN 0 AND 511),
	owner_uid INTEGER CHECK (owner_uid IS NULL OR owner_uid BETWEEN 0 AND 4294967295),
	group_gid INTEGER CHECK (group_gid IS NULL OR group_gid BETWEEN 0 AND 4294967295),
	operations_json TEXT NOT NULL DEFAULT '[]'
		CHECK (json_valid(operations_json) AND json_type(operations_json) = 'array'),
	duplicate_scan_id TEXT,
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
INSERT INTO agent_conversations SELECT * FROM agent_conversations_v22;
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
INSERT INTO agent_conversation_messages SELECT * FROM agent_conversation_messages_v22;

CREATE TABLE duplicate_scan_runs (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE RESTRICT,
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	scope_path TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
	phase TEXT NOT NULL CHECK (phase IN ('enumerating', 'hashing', 'complete')),
	entries_visited INTEGER NOT NULL DEFAULT 0 CHECK (entries_visited >= 0),
	candidate_files INTEGER NOT NULL DEFAULT 0 CHECK (candidate_files >= 0),
	files_hashed INTEGER NOT NULL DEFAULT 0 CHECK (files_hashed >= 0),
	bytes_hashed INTEGER NOT NULL DEFAULT 0 CHECK (bytes_hashed >= 0),
	total_bytes INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
	group_count INTEGER NOT NULL DEFAULT 0 CHECK (group_count >= 0),
	duplicate_file_count INTEGER NOT NULL DEFAULT 0 CHECK (duplicate_file_count >= 0),
	reclaimable_bytes INTEGER NOT NULL DEFAULT 0 CHECK (reclaimable_bytes >= 0),
	error_code TEXT NOT NULL DEFAULT '',
	cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);
INSERT INTO duplicate_scan_runs SELECT * FROM duplicate_scan_runs_v22;
CREATE INDEX duplicate_scan_runs_conversation ON duplicate_scan_runs(conversation_id, created_at_ns DESC);

CREATE TABLE duplicate_scan_groups (
	scan_id TEXT NOT NULL REFERENCES duplicate_scan_runs(id) ON DELETE CASCADE,
	group_id INTEGER NOT NULL CHECK (group_id > 0),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	duplicate_count INTEGER NOT NULL CHECK (duplicate_count >= 2),
	PRIMARY KEY (scan_id, group_id)
);
INSERT INTO duplicate_scan_groups SELECT * FROM duplicate_scan_groups_v22;

CREATE TABLE duplicate_scan_files (
	scan_id TEXT NOT NULL,
	group_id INTEGER NOT NULL,
	path TEXT NOT NULL CHECK (path <> '' AND substr(path, 1, 1) <> '/'),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	modified_at_ns INTEGER NOT NULL,
	PRIMARY KEY (scan_id, group_id, path),
	FOREIGN KEY (scan_id, group_id) REFERENCES duplicate_scan_groups(scan_id, group_id) ON DELETE CASCADE
);
INSERT INTO duplicate_scan_files SELECT * FROM duplicate_scan_files_v22;
CREATE INDEX duplicate_scan_files_path ON duplicate_scan_files(scan_id, group_id, path);

DROP TABLE agent_conversations_v22;
DROP TABLE agent_conversation_messages_v22;
DROP TABLE duplicate_scan_runs_v22;
DROP TABLE duplicate_scan_groups_v22;
DROP TABLE duplicate_scan_files_v22;

CREATE TABLE shell_runs (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	plan_id TEXT NOT NULL UNIQUE REFERENCES plans(id) ON DELETE RESTRICT,
	operation_id TEXT NOT NULL UNIQUE REFERENCES operations(id) ON DELETE RESTRICT,
	command TEXT NOT NULL CHECK (length(command) > 0),
	working_directory TEXT NOT NULL,
	stage_id TEXT NOT NULL UNIQUE CHECK (stage_id <> ''),
	status TEXT NOT NULL CHECK (status IN ('awaiting_approval', 'approved', 'executing', 'completed', 'failed', 'rejected', 'cancelled', 'invalidated', 'partial')),
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);

CREATE TABLE shell_changes (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	plan_id TEXT NOT NULL REFERENCES plans(id) ON DELETE RESTRICT,
	ordinal INTEGER NOT NULL CHECK (ordinal >= 0 AND ordinal < 20000),
	path TEXT NOT NULL CHECK (path <> '' AND substr(path, 1, 1) <> '/'),
	kind TEXT NOT NULL CHECK (kind IN ('added', 'modified', 'deleted')),
	original_state_json TEXT NOT NULL CHECK (json_valid(original_state_json) AND json_type(original_state_json) = 'object'),
	candidate_sha256 TEXT CHECK (candidate_sha256 IS NULL OR length(candidate_sha256) = 64),
	candidate_size INTEGER CHECK (candidate_size IS NULL OR candidate_size >= 0),
	candidate_mode INTEGER CHECK (candidate_mode IS NULL OR candidate_mode BETWEEN 0 AND 511),
	binary INTEGER NOT NULL CHECK (binary IN (0, 1)),
	truncated INTEGER NOT NULL CHECK (truncated IN (0, 1)),
	payload_id TEXT NOT NULL DEFAULT '',
	approval_state TEXT NOT NULL CHECK (approval_state IN ('pending', 'approved', 'rejected', 'invalidated')),
	approved_by TEXT NOT NULL DEFAULT '',
	approved_at_ns INTEGER,
	execution_status TEXT NOT NULL CHECK (execution_status IN ('pending', 'running', 'succeeded', 'failed', 'skipped', 'partial', 'cancelled')),
	result_message TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	CHECK (
		(approval_state = 'approved' AND approved_by <> '' AND approved_at_ns IS NOT NULL)
		OR (approval_state <> 'approved' AND approved_by = '' AND approved_at_ns IS NULL)
	),
	CHECK (
		(kind = 'deleted' AND candidate_sha256 IS NULL AND candidate_size IS NULL AND candidate_mode IS NULL AND payload_id = '')
		OR (kind IN ('added', 'modified') AND candidate_mode IS NOT NULL AND
			((candidate_sha256 IS NULL AND candidate_size IS NULL AND payload_id = '') OR
			 (candidate_sha256 IS NOT NULL AND candidate_size IS NOT NULL AND payload_id <> '')))
	),
	UNIQUE(plan_id, ordinal),
	UNIQUE(plan_id, path)
);
CREATE INDEX shell_changes_plan_approval ON shell_changes(plan_id, approval_state, ordinal);
CREATE INDEX shell_changes_payload ON shell_changes(payload_id) WHERE payload_id <> '';
`,
	},
	{
		version: 24,
		// 增加显式文本文件新建与覆盖动作，保留现有会话和扫描历史。
		sql: `
CREATE TEMP TABLE agent_conversations_v23 AS SELECT * FROM agent_conversations;
CREATE TEMP TABLE agent_conversation_messages_v23 AS SELECT * FROM agent_conversation_messages;
CREATE TEMP TABLE duplicate_scan_runs_v23 AS SELECT * FROM duplicate_scan_runs;
CREATE TEMP TABLE duplicate_scan_groups_v23 AS SELECT * FROM duplicate_scan_groups;
CREATE TEMP TABLE duplicate_scan_files_v23 AS SELECT * FROM duplicate_scan_files;

DROP TABLE duplicate_scan_files;
DROP TABLE duplicate_scan_groups;
DROP TABLE duplicate_scan_runs;
DROP TABLE agent_conversation_messages;
DROP TABLE agent_conversations;

CREATE TABLE agent_conversations (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	state TEXT NOT NULL CHECK (state IN ('processing', 'clarifying', 'awaiting_confirmation', 'analyzing', 'awaiting_approval', 'analysis_complete', 'unsupported', 'failed')),
	goal TEXT NOT NULL DEFAULT '',
	file_path TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	destination TEXT NOT NULL DEFAULT '',
	task_id TEXT REFERENCES tasks(id) ON DELETE RESTRICT,
	plan_id TEXT REFERENCES plans(id) ON DELETE RESTRICT,
	error_code TEXT NOT NULL DEFAULT '',
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL,
	analysis_action TEXT NOT NULL DEFAULT 'rename'
		CHECK (analysis_action IN ('rename', 'edit_text', 'create_file', 'overwrite_file', 'create_directory', 'move_file', 'create_symlink', 'copy_file', 'copy_directory', 'create_archive', 'extract_archive', 'set_permissions', 'set_owner', 'find_duplicates', 'archive_photos', 'classify_documents', 'trash', 'permanent_delete', 'run_shell')),
	target_path TEXT NOT NULL DEFAULT '',
	mode INTEGER CHECK (mode IS NULL OR mode BETWEEN 0 AND 511),
	owner_uid INTEGER CHECK (owner_uid IS NULL OR owner_uid BETWEEN 0 AND 4294967295),
	group_gid INTEGER CHECK (group_gid IS NULL OR group_gid BETWEEN 0 AND 4294967295),
	operations_json TEXT NOT NULL DEFAULT '[]'
		CHECK (json_valid(operations_json) AND json_type(operations_json) = 'array'),
	duplicate_scan_id TEXT,
	CHECK ((task_id IS NULL AND plan_id IS NULL) OR (task_id IS NOT NULL AND plan_id IS NOT NULL))
);
INSERT INTO agent_conversations SELECT * FROM agent_conversations_v23;
CREATE INDEX agent_conversations_updated ON agent_conversations(updated_at_ns DESC);

CREATE TABLE agent_conversation_messages (
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	sequence INTEGER NOT NULL CHECK (sequence > 0),
	role TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
	content TEXT NOT NULL CHECK (length(content) > 0 AND length(content) <= 4096),
	created_at_ns INTEGER NOT NULL,
	PRIMARY KEY (conversation_id, sequence)
);
INSERT INTO agent_conversation_messages SELECT * FROM agent_conversation_messages_v23;

CREATE TABLE duplicate_scan_runs (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	task_id TEXT NOT NULL UNIQUE REFERENCES tasks(id) ON DELETE RESTRICT,
	conversation_id TEXT NOT NULL REFERENCES agent_conversations(id) ON DELETE CASCADE,
	scope_path TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
	phase TEXT NOT NULL CHECK (phase IN ('enumerating', 'hashing', 'complete')),
	entries_visited INTEGER NOT NULL DEFAULT 0 CHECK (entries_visited >= 0),
	candidate_files INTEGER NOT NULL DEFAULT 0 CHECK (candidate_files >= 0),
	files_hashed INTEGER NOT NULL DEFAULT 0 CHECK (files_hashed >= 0),
	bytes_hashed INTEGER NOT NULL DEFAULT 0 CHECK (bytes_hashed >= 0),
	total_bytes INTEGER NOT NULL DEFAULT 0 CHECK (total_bytes >= 0),
	group_count INTEGER NOT NULL DEFAULT 0 CHECK (group_count >= 0),
	duplicate_file_count INTEGER NOT NULL DEFAULT 0 CHECK (duplicate_file_count >= 0),
	reclaimable_bytes INTEGER NOT NULL DEFAULT 0 CHECK (reclaimable_bytes >= 0),
	error_code TEXT NOT NULL DEFAULT '',
	cancel_requested INTEGER NOT NULL DEFAULT 0 CHECK (cancel_requested IN (0, 1)),
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);
INSERT INTO duplicate_scan_runs SELECT * FROM duplicate_scan_runs_v23;
CREATE INDEX duplicate_scan_runs_conversation ON duplicate_scan_runs(conversation_id, created_at_ns DESC);

CREATE TABLE duplicate_scan_groups (
	scan_id TEXT NOT NULL REFERENCES duplicate_scan_runs(id) ON DELETE CASCADE,
	group_id INTEGER NOT NULL CHECK (group_id > 0),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	duplicate_count INTEGER NOT NULL CHECK (duplicate_count >= 2),
	PRIMARY KEY (scan_id, group_id)
);
INSERT INTO duplicate_scan_groups SELECT * FROM duplicate_scan_groups_v23;

CREATE TABLE duplicate_scan_files (
	scan_id TEXT NOT NULL,
	group_id INTEGER NOT NULL,
	path TEXT NOT NULL CHECK (path <> '' AND substr(path, 1, 1) <> '/'),
	size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
	modified_at_ns INTEGER NOT NULL,
	PRIMARY KEY (scan_id, group_id, path),
	FOREIGN KEY (scan_id, group_id) REFERENCES duplicate_scan_groups(scan_id, group_id) ON DELETE CASCADE
);
INSERT INTO duplicate_scan_files SELECT * FROM duplicate_scan_files_v23;
CREATE INDEX duplicate_scan_files_path ON duplicate_scan_files(scan_id, group_id, path);

DROP TABLE agent_conversations_v23;
DROP TABLE agent_conversation_messages_v23;
DROP TABLE duplicate_scan_runs_v23;
DROP TABLE duplicate_scan_groups_v23;
DROP TABLE duplicate_scan_files_v23;
`,
	},
	{
		version: 25,
		// 保存全局 Agent 权限偏好及每个方案的审计模式。
		sql: `
ALTER TABLE plans ADD COLUMN execution_mode TEXT NOT NULL DEFAULT ''
	CHECK (execution_mode IN ('', 'bypass', 'manual'));
ALTER TABLE plans ADD COLUMN reviewed_at_ns INTEGER;

CREATE TABLE agent_settings (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	bypass_permission INTEGER NOT NULL CHECK (bypass_permission IN (0, 1)),
	updated_at_ns INTEGER NOT NULL
);
INSERT INTO agent_settings (id, bypass_permission, updated_at_ns)
VALUES (1, 1, CAST(strftime('%s', 'now') AS INTEGER) * 1000000000);
`,
	},
	{
		version: 26,
		// 保存有上限的 Shell 输出，支持恢复历史工具调用。
		sql: `
ALTER TABLE shell_runs ADD COLUMN exit_code INTEGER NOT NULL DEFAULT 0 CHECK (exit_code BETWEEN 0 AND 255);
ALTER TABLE shell_runs ADD COLUMN stdout TEXT NOT NULL DEFAULT '';
ALTER TABLE shell_runs ADD COLUMN stderr TEXT NOT NULL DEFAULT '';
ALTER TABLE shell_runs ADD COLUMN output_available INTEGER NOT NULL DEFAULT 0 CHECK (output_available IN (0, 1));
`,
	},
	{
		version: 27,
		// 删除 Shell 原始输出，仅保留退出码和输出省略标记。
		sql: `
CREATE TABLE shell_runs_v27 (
	id TEXT PRIMARY KEY CHECK (id <> ''),
	plan_id TEXT NOT NULL UNIQUE REFERENCES plans(id) ON DELETE RESTRICT,
	operation_id TEXT NOT NULL UNIQUE REFERENCES operations(id) ON DELETE RESTRICT,
	command TEXT NOT NULL CHECK (length(command) > 0),
	working_directory TEXT NOT NULL,
	output_discarded INTEGER NOT NULL CHECK (output_discarded IN (0, 1)),
	exit_code INTEGER NOT NULL CHECK (exit_code BETWEEN 0 AND 255),
	stage_id TEXT NOT NULL UNIQUE CHECK (stage_id <> ''),
	status TEXT NOT NULL CHECK (status IN ('awaiting_approval', 'approved', 'executing', 'completed', 'failed', 'rejected', 'cancelled', 'invalidated', 'partial')),
	created_at_ns INTEGER NOT NULL,
	updated_at_ns INTEGER NOT NULL
);
INSERT INTO shell_runs_v27 (
	id, plan_id, operation_id, command, working_directory, output_discarded,
	exit_code, stage_id, status, created_at_ns, updated_at_ns
)
SELECT id, plan_id, operation_id, command, working_directory,
	CASE WHEN length(CAST(stdout AS BLOB)) > 0 OR length(CAST(stderr AS BLOB)) > 0 THEN 1 ELSE 0 END,
	exit_code, stage_id, status, created_at_ns, updated_at_ns
FROM shell_runs;
UPDATE shell_runs SET stdout = '', stderr = '';
DROP TABLE shell_runs;
ALTER TABLE shell_runs_v27 RENAME TO shell_runs;
`,
	},
	{
		version: 28,
		// 保存 Agent 会话的受限摘要，用于长对话上下文压缩。
		sql: `
ALTER TABLE agent_conversations ADD COLUMN context_summary TEXT NOT NULL DEFAULT ''
	CHECK (length(CAST(context_summary AS BLOB)) <= 4096);
`,
	},
	{
		version: 29,
		// 用复合索引覆盖默认目录排序和递归搜索排序。
		sql: `
DROP INDEX file_index_generation_parent_name;
CREATE INDEX file_index_generation_parent_order_name
ON file_index_generation_entries(
	generation, parent_path,
	(CASE WHEN kind = 'directory' THEN 0 ELSE 1 END),
	name COLLATE NOCASE, relative_path
);
CREATE INDEX file_index_generation_order_name
ON file_index_generation_entries(
	generation,
	(CASE WHEN kind = 'directory' THEN 0 ELSE 1 END),
	name COLLATE NOCASE, relative_path
);
`,
	},
	{
		version: 30,
		// 保存限长 Shell 输出，并记录仅供下一轮模型使用的回执来源。
		sql: `
ALTER TABLE shell_runs ADD COLUMN stdout TEXT NOT NULL DEFAULT ''
	CHECK (length(CAST(stdout AS BLOB)) <= 16384);
ALTER TABLE shell_runs ADD COLUMN stderr TEXT NOT NULL DEFAULT ''
	CHECK (length(CAST(stderr AS BLOB)) <= 16384);
ALTER TABLE shell_runs ADD COLUMN output_truncated INTEGER NOT NULL DEFAULT 0
	CHECK (output_truncated IN (0, 1));
ALTER TABLE agent_conversations ADD COLUMN tool_context_plan_id TEXT NOT NULL DEFAULT ''
	CHECK (length(tool_context_plan_id) <= 256);
`,
	},
}

// migrate 顺序应用迁移，并清理旧 Shell 输出的 SQLite 残留。
func migrate(ctx context.Context, db *sql.DB) error {
	// 固定单一连接，确保安全删除设置覆盖迁移事务。
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("get migration connection: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY CHECK (version > 0),
			applied_at_ns INTEGER NOT NULL
		)`); err != nil {
		return fmt.Errorf("create migration ledger: %w", err)
	}

	var current int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("read current schema version: %w", err)
	}
	if current > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this application supports (latest %d)", current, len(migrations))
	}

	clearShellOutput := current >= 27
	for _, next := range migrations {
		if next.version <= current {
			continue
		}
		if next.version == 27 {
			// 覆盖旧输出后再删表，避免正文留在空闲页。
			if _, err := conn.ExecContext(ctx, `PRAGMA secure_delete = ON`); err != nil {
				return fmt.Errorf("enable secure deletion for migration %d: %w", next.version, err)
			}
			var enabled int
			if err := conn.QueryRowContext(ctx, `PRAGMA secure_delete`).Scan(&enabled); err != nil {
				return fmt.Errorf("verify secure deletion for migration %d: %w", next.version, err)
			}
			if enabled != 1 {
				return fmt.Errorf("secure deletion is disabled for migration %d", next.version)
			}
			clearShellOutput = true
		}
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", next.version, err)
		}
		if _, err := tx.ExecContext(ctx, next.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", next.version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at_ns) VALUES (?, ?)`, next.version, time.Now().UTC().UnixNano()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", next.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", next.version, err)
		}
	}
	if clearShellOutput {
		var busy, logFrames, checkpointedFrames int
		if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointedFrames); err != nil {
			return fmt.Errorf("clear Shell output from SQLite WAL: %w", err)
		}
		if busy != 0 || (logFrames >= 0 && checkpointedFrames < logFrames) {
			return fmt.Errorf("clear Shell output from SQLite WAL: checkpoint incomplete (busy=%d log=%d checkpointed=%d)", busy, logFrames, checkpointedFrames)
		}
	}
	return nil
}

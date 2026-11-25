-- Reference schema for a freshly initialized Bambuddy v.1.3.2 SQLite database.
-- Source commit b7d894b57c0a373f66dc0de49680865b02b3aae5.
-- Contains database definitions only. Application seeds, data, migrations and
-- SQLite-generated indexes and FTS5 shadow tables are supplied by their owners.
-- The application also supports PostgreSQL, with dialect-specific migrations.

CREATE TABLE active_print_sessions (
	printer_id INTEGER NOT NULL, 
	print_name VARCHAR NOT NULL, 
	started_at DATETIME NOT NULL, 
	tray_now_at_start INTEGER NOT NULL, 
	plate_id INTEGER, 
	ams_mapping JSON, 
	spool_assignments JSON, 
	tray_remain_start JSON, 
	tray_change_log JSON, 
	spool_change_log JSON, 
	PRIMARY KEY (printer_id), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE active_print_spoolman (
	id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	archive_id INTEGER NOT NULL, 
	filament_usage JSON, 
	ams_trays JSON NOT NULL, 
	slot_to_tray JSON, 
	layer_usage JSON, 
	filament_properties JSON, 
	tray_remain_start JSON, 
	tray_now_at_start INTEGER, 
	PRIMARY KEY (id), 
	CONSTRAINT uq_printer_archive UNIQUE (printer_id, archive_id), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE, 
	FOREIGN KEY(archive_id) REFERENCES print_archives (id) ON DELETE CASCADE
);

CREATE TABLE ams_labels (
	id INTEGER NOT NULL, 
	ams_serial_number VARCHAR(50) NOT NULL, 
	ams_id INTEGER, 
	label VARCHAR(100) NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT uq_ams_label_serial UNIQUE (ams_serial_number)
);

CREATE TABLE ams_sensor_history (
	id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	ams_id INTEGER NOT NULL, 
	humidity FLOAT, 
	humidity_raw FLOAT, 
	temperature FLOAT, 
	recorded_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE announcement_reads (
	id INTEGER NOT NULL, 
	announcement_id INTEGER NOT NULL, 
	user_id INTEGER, 
	read_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT uq_announcement_reads_user UNIQUE (announcement_id, user_id), 
	FOREIGN KEY(announcement_id) REFERENCES announcements (id) ON DELETE CASCADE, 
	FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);

CREATE TABLE announcements (
	id INTEGER NOT NULL, 
	public_id VARCHAR(64) NOT NULL, 
	level VARCHAR(16) NOT NULL, 
	texts TEXT NOT NULL, 
	link_url VARCHAR(500), 
	published_at DATETIME, 
	expires_at DATETIME, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE api_keys (
	id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, 
	name VARCHAR(100) NOT NULL, 
	key_hash VARCHAR(255) NOT NULL, 
	key_prefix VARCHAR(20) NOT NULL, 
	user_id INTEGER, 
	can_queue BOOLEAN NOT NULL, 
	can_control_printer BOOLEAN NOT NULL, 
	can_read_status BOOLEAN NOT NULL, 
	can_manage_library BOOLEAN DEFAULT 1 NOT NULL, 
	can_manage_inventory BOOLEAN DEFAULT 1 NOT NULL, 
	can_manage_maintenance BOOLEAN DEFAULT 1 NOT NULL, 
	can_manage_archives BOOLEAN DEFAULT 1 NOT NULL, 
	can_manage_projects BOOLEAN DEFAULT 1 NOT NULL, 
	can_access_cloud BOOLEAN DEFAULT 0 NOT NULL, 
	can_update_energy_cost BOOLEAN DEFAULT 0 NOT NULL, 
	can_send_notifications BOOLEAN DEFAULT 0 NOT NULL, 
	printer_ids JSON, 
	enabled BOOLEAN NOT NULL, 
	last_used DATETIME, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	expires_at DATETIME, 
	FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);

CREATE TABLE archive_aliases (
	merged_archive_id INTEGER NOT NULL, 
	archive_id INTEGER NOT NULL, 
	merged_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (merged_archive_id), 
	FOREIGN KEY(archive_id) REFERENCES print_archives (id) ON DELETE CASCADE
);

CREATE VIRTUAL TABLE archive_fts USING fts5(
                    print_name,
                    filename,
                    tags,
                    notes,
                    designer,
                    filament_type,
                    content='print_archives',
                    content_rowid='id'
                );

CREATE TABLE auth_ephemeral_tokens (
	id INTEGER NOT NULL, 
	token VARCHAR(128) NOT NULL, 
	token_type VARCHAR(20) NOT NULL, 
	username VARCHAR(150), 
	provider_id INTEGER, 
	api_key_id INTEGER, 
	nonce VARCHAR(128), 
	code_verifier VARCHAR(128), 
	challenge_id VARCHAR(128), 
	expires_at DATETIME NOT NULL, 
	created_at DATETIME NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE auth_rate_limit_events (
	id INTEGER NOT NULL, 
	username VARCHAR(150) NOT NULL, 
	event_type VARCHAR(20) NOT NULL, 
	occurred_at DATETIME NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE budget_reservations (
	id INTEGER NOT NULL, 
	cost_center_id INTEGER NOT NULL, 
	amount NUMERIC(14, 2) NOT NULL, 
	status VARCHAR(20) NOT NULL, 
	source_type VARCHAR(50) NOT NULL, 
	source_id INTEGER, 
	print_archive_id INTEGER, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	released_at DATETIME, 
	PRIMARY KEY (id), 
	FOREIGN KEY(cost_center_id) REFERENCES cost_centers (id) ON DELETE CASCADE, 
	FOREIGN KEY(print_archive_id) REFERENCES print_archives (id) ON DELETE SET NULL
);

CREATE TABLE bug_reports (
	id INTEGER NOT NULL, 
	description TEXT NOT NULL, 
	reporter_email VARCHAR(255), 
	github_issue_number INTEGER, 
	github_issue_url VARCHAR(500), 
	status VARCHAR(20) NOT NULL, 
	error_message TEXT, 
	email_sent BOOLEAN NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE color_catalog (
	id INTEGER NOT NULL, 
	manufacturer VARCHAR(200) NOT NULL, 
	color_name VARCHAR(200) NOT NULL, 
	hex_color VARCHAR(9) NOT NULL, 
	material VARCHAR(100), 
	is_default BOOLEAN NOT NULL, 
	extra_colors VARCHAR(255), 
	effect_type VARCHAR(20), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE connected_app_grants (
	id INTEGER NOT NULL, 
	app_id INTEGER NOT NULL, 
	user_id INTEGER NOT NULL, 
	granted_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(app_id) REFERENCES connected_apps (id) ON DELETE CASCADE, 
	FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);

CREATE TABLE connected_apps (
	id INTEGER NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	client_id VARCHAR(64) NOT NULL, 
	client_secret_hash VARCHAR(255) NOT NULL, 
	redirect_uri VARCHAR(500) NOT NULL, 
	enabled BOOLEAN NOT NULL, 
	created_by_id INTEGER, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	last_used_at DATETIME, 
	PRIMARY KEY (id), 
	FOREIGN KEY(created_by_id) REFERENCES users (id) ON DELETE SET NULL
);

CREATE TABLE cost_center_members (
	id INTEGER NOT NULL, 
	cost_center_id INTEGER NOT NULL, 
	user_id INTEGER NOT NULL, 
	can_print BOOLEAN NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT uq_cost_center_members_cc_user UNIQUE (cost_center_id, user_id), 
	FOREIGN KEY(cost_center_id) REFERENCES cost_centers (id) ON DELETE CASCADE, 
	FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);

CREATE TABLE cost_centers (
	id INTEGER NOT NULL, 
	code VARCHAR(32) NOT NULL, 
	name VARCHAR(150) NOT NULL, 
	is_active BOOLEAN NOT NULL, 
	is_private BOOLEAN NOT NULL, 
	owner_user_id INTEGER, 
	total_budget NUMERIC(14, 2), 
	monthly_budget NUMERIC(14, 2), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(owner_user_id) REFERENCES users (id) ON DELETE SET NULL
);

CREATE TABLE external_link_badges (
	link_id INTEGER NOT NULL, 
	count INTEGER NOT NULL, 
	updated_at DATETIME NOT NULL, 
	PRIMARY KEY (link_id), 
	FOREIGN KEY(link_id) REFERENCES external_links (id) ON DELETE CASCADE
);

CREATE TABLE external_links (
	id INTEGER NOT NULL, 
	name VARCHAR(50) NOT NULL, 
	url VARCHAR(500) NOT NULL, 
	icon VARCHAR(50) NOT NULL, 
	custom_icon VARCHAR(255), 
	open_in_new_tab BOOLEAN NOT NULL, 
	sort_order INTEGER NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE filament_profile_default (
	id INTEGER NOT NULL, 
	match_key VARCHAR(320) NOT NULL, 
	brand VARCHAR(100) NOT NULL, 
	material VARCHAR(50) NOT NULL, 
	subtype VARCHAR(50) NOT NULL, 
	color_name VARCHAR(100) NOT NULL, 
	slicer_filament VARCHAR(128), 
	slicer_filament_name VARCHAR(255), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	UNIQUE (match_key)
);

CREATE TABLE filament_profile_default_k_profile (
	id INTEGER NOT NULL, 
	profile_default_id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	extruder INTEGER NOT NULL, 
	nozzle_diameter VARCHAR(10) NOT NULL, 
	nozzle_type VARCHAR(50), 
	k_value FLOAT NOT NULL, 
	name VARCHAR(100), 
	cali_idx INTEGER, 
	setting_id VARCHAR(50), 
	PRIMARY KEY (id), 
	UNIQUE (profile_default_id, printer_id, extruder, nozzle_diameter), 
	FOREIGN KEY(profile_default_id) REFERENCES filament_profile_default (id) ON DELETE CASCADE, 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE filament_profile_default_preset (
	id INTEGER NOT NULL, 
	profile_default_id INTEGER NOT NULL, 
	printer_model VARCHAR(50) NOT NULL, 
	nozzle_diameter VARCHAR(10) NOT NULL, 
	slicer_filament VARCHAR(128), 
	slicer_filament_name VARCHAR(255), 
	PRIMARY KEY (id), 
	UNIQUE (profile_default_id, printer_model, nozzle_diameter), 
	FOREIGN KEY(profile_default_id) REFERENCES filament_profile_default (id) ON DELETE CASCADE
);

CREATE TABLE filament_shopping_list (
	id INTEGER NOT NULL, 
	material VARCHAR(50) NOT NULL, 
	subtype VARCHAR(50), 
	brand VARCHAR(100), 
	color_name VARCHAR(100), 
	quantity_spools INTEGER NOT NULL, 
	note VARCHAR(500), 
	status VARCHAR(20) NOT NULL, 
	purchased_at DATETIME, 
	added_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE filament_sku_settings (
	id INTEGER NOT NULL, 
	material VARCHAR(50) NOT NULL, 
	subtype VARCHAR(50), 
	brand VARCHAR(100), 
	color_name VARCHAR(100), 
	lead_time_days INTEGER NOT NULL, 
	safety_margin_value INTEGER NOT NULL, 
	safety_margin_unit VARCHAR(10) NOT NULL, 
	alerts_snoozed BOOLEAN NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT uq_filament_sku UNIQUE (material, subtype, brand, color_name)
);

CREATE TABLE filaments (
	id INTEGER NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	type VARCHAR(50) NOT NULL, 
	brand VARCHAR(100), 
	color VARCHAR(50), 
	color_hex VARCHAR(7), 
	cost_per_kg FLOAT NOT NULL, 
	spool_weight_g FLOAT NOT NULL, 
	currency VARCHAR(3) NOT NULL, 
	density FLOAT, 
	print_temp_min INTEGER, 
	print_temp_max INTEGER, 
	bed_temp_min INTEGER, 
	bed_temp_max INTEGER, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE file_variant_groups (
	id INTEGER NOT NULL, 
	name VARCHAR(255) NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	created_by_id INTEGER, 
	PRIMARY KEY (id), 
	FOREIGN KEY(created_by_id) REFERENCES users (id) ON DELETE SET NULL
);

CREATE TABLE github_backup_config (
	id INTEGER NOT NULL, 
	repository_url VARCHAR(500) NOT NULL, 
	access_token TEXT NOT NULL, 
	branch VARCHAR(100) NOT NULL, 
	provider VARCHAR(30) DEFAULT 'github' NOT NULL, 
	allow_insecure_http BOOLEAN DEFAULT 0 NOT NULL, 
	schedule_enabled BOOLEAN NOT NULL, 
	schedule_type VARCHAR(20) NOT NULL, 
	schedule_cron VARCHAR(100), 
	backup_kprofiles BOOLEAN NOT NULL, 
	backup_cloud_profiles BOOLEAN NOT NULL, 
	backup_settings BOOLEAN NOT NULL, 
	backup_spools BOOLEAN NOT NULL, 
	backup_archives BOOLEAN NOT NULL, 
	enabled BOOLEAN NOT NULL, 
	last_backup_at DATETIME, 
	last_backup_status VARCHAR(20), 
	last_backup_message TEXT, 
	last_backup_commit_sha VARCHAR(40), 
	next_scheduled_run DATETIME, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE github_backup_logs (
	id INTEGER NOT NULL, 
	config_id INTEGER NOT NULL, 
	started_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	completed_at DATETIME, 
	status VARCHAR(20) NOT NULL, 
	"trigger" VARCHAR(20) NOT NULL, 
	commit_sha VARCHAR(40), 
	files_changed INTEGER NOT NULL, 
	error_message TEXT, 
	PRIMARY KEY (id), 
	FOREIGN KEY(config_id) REFERENCES github_backup_config (id) ON DELETE CASCADE
);

CREATE TABLE group_locations (
	group_id INTEGER NOT NULL, 
	location VARCHAR(100) NOT NULL, 
	PRIMARY KEY (group_id, location), 
	FOREIGN KEY(group_id) REFERENCES groups (id) ON DELETE CASCADE
);

CREATE TABLE group_printers (
	group_id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	PRIMARY KEY (group_id, printer_id), 
	FOREIGN KEY(group_id) REFERENCES groups (id) ON DELETE CASCADE, 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE groups (
	id INTEGER NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	description VARCHAR(500), 
	permissions JSON NOT NULL, 
	is_system BOOLEAN NOT NULL, 
	restrict_printers BOOLEAN DEFAULT '0' NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE hms_notified_faults (
	id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	fault_key VARCHAR(64) NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT uq_hms_notified_faults_printer_fault UNIQUE (printer_id, fault_key), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE kprofile_notes (
	id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	setting_id VARCHAR(100) NOT NULL, 
	note TEXT NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE label_templates (
	id INTEGER NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	name_key VARCHAR(300) NOT NULL, 
	page VARCHAR(10) NOT NULL, 
	label_w_mm FLOAT NOT NULL, 
	label_h_mm FLOAT NOT NULL, 
	cols INTEGER NOT NULL, 
	rows INTEGER NOT NULL, 
	top_mm FLOAT NOT NULL, 
	left_mm FLOAT NOT NULL, 
	col_gap_mm FLOAT NOT NULL, 
	row_gap_mm FLOAT NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE library_file_tags (
	file_id INTEGER NOT NULL, 
	tag_id INTEGER NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (file_id, tag_id), 
	FOREIGN KEY(file_id) REFERENCES library_files (id) ON DELETE CASCADE, 
	FOREIGN KEY(tag_id) REFERENCES library_tags (id) ON DELETE CASCADE
);

CREATE TABLE library_files (
	id INTEGER NOT NULL, 
	folder_id INTEGER, 
	project_id INTEGER, 
	is_external BOOLEAN NOT NULL, 
	filename VARCHAR(255) NOT NULL, 
	file_path VARCHAR(500) NOT NULL, 
	file_type VARCHAR(10) NOT NULL, 
	file_size INTEGER NOT NULL, 
	file_hash VARCHAR(64), 
	thumbnail_path VARCHAR(500), 
	file_metadata JSON, 
	print_count INTEGER NOT NULL, 
	last_printed_at DATETIME, 
	notes TEXT, 
	external_url VARCHAR(500), 
	photos JSON, 
	source_type VARCHAR(32), 
	source_url VARCHAR(512), 
	variant_group_id INTEGER, 
	variant_position INTEGER DEFAULT '0' NOT NULL, 
	variant_target_model VARCHAR(50), 
	created_by_id INTEGER, 
	deleted_at DATETIME, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	fs_modified_at DATETIME, 
	PRIMARY KEY (id), 
	FOREIGN KEY(folder_id) REFERENCES library_folders (id) ON DELETE CASCADE, 
	FOREIGN KEY(project_id) REFERENCES projects (id) ON DELETE SET NULL, 
	FOREIGN KEY(variant_group_id) REFERENCES file_variant_groups (id) ON DELETE SET NULL, 
	FOREIGN KEY(created_by_id) REFERENCES users (id) ON DELETE SET NULL
);

CREATE TABLE library_folders (
	id INTEGER NOT NULL, 
	name VARCHAR(255) NOT NULL, 
	parent_id INTEGER, 
	is_external BOOLEAN NOT NULL, 
	external_readonly BOOLEAN NOT NULL, 
	external_show_hidden BOOLEAN NOT NULL, 
	external_path VARCHAR(500), 
	created_by_id INTEGER, 
	shared BOOLEAN DEFAULT '0' NOT NULL, 
	project_id INTEGER, 
	archive_id INTEGER, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	fs_modified_at DATETIME, 
	PRIMARY KEY (id), 
	FOREIGN KEY(parent_id) REFERENCES library_folders (id) ON DELETE CASCADE, 
	FOREIGN KEY(created_by_id) REFERENCES users (id) ON DELETE SET NULL, 
	FOREIGN KEY(project_id) REFERENCES projects (id) ON DELETE SET NULL, 
	CONSTRAINT fk_library_folders_archive_id FOREIGN KEY(archive_id) REFERENCES print_archives (id) ON DELETE SET NULL
);

CREATE TABLE library_tags (
	id INTEGER NOT NULL, 
	name VARCHAR(64) NOT NULL, 
	name_key VARCHAR(64) NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE local_presets (
	id INTEGER NOT NULL, 
	name VARCHAR(300) NOT NULL, 
	preset_type VARCHAR(20) NOT NULL, 
	source VARCHAR(50) NOT NULL, 
	filament_type VARCHAR(50), 
	filament_vendor VARCHAR(200), 
	nozzle_temp_min INTEGER, 
	nozzle_temp_max INTEGER, 
	pressure_advance VARCHAR(50), 
	default_filament_colour VARCHAR(50), 
	filament_cost VARCHAR(50), 
	filament_density VARCHAR(50), 
	compatible_printers TEXT, 
	setting TEXT NOT NULL, 
	inherits VARCHAR(300), 
	version VARCHAR(50), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE location_ha_sensors (
	id INTEGER NOT NULL, 
	location_id INTEGER NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	entity_id VARCHAR(255) NOT NULL, 
	kind VARCHAR(16) NOT NULL, 
	device_class VARCHAR(32), 
	unit VARCHAR(16), 
	alert_state VARCHAR(8), 
	alert_above FLOAT, 
	alert_below FLOAT, 
	notify_on_alert BOOLEAN DEFAULT '0' NOT NULL, 
	show_on_card BOOLEAN DEFAULT '1' NOT NULL, 
	sort_order INTEGER DEFAULT '0' NOT NULL, 
	last_state VARCHAR(64), 
	last_changed DATETIME, 
	last_checked DATETIME, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(location_id) REFERENCES locations (id) ON DELETE CASCADE
);

CREATE TABLE locations (
	id INTEGER NOT NULL, 
	name VARCHAR(255) NOT NULL, 
	name_key VARCHAR(255) NOT NULL, 
	identifier VARCHAR(100), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	UNIQUE (name)
);

CREATE TABLE long_lived_tokens (
	id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, 
	user_id INTEGER NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	lookup_prefix VARCHAR(8) NOT NULL, 
	secret_hash VARCHAR(255) NOT NULL, 
	scope VARCHAR(32) NOT NULL, 
	expires_at DATETIME NOT NULL, 
	last_used_at DATETIME, 
	revoked_at DATETIME, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);

CREATE TABLE maintenance_history (
	id INTEGER NOT NULL, 
	printer_maintenance_id INTEGER NOT NULL, 
	performed_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	hours_at_maintenance FLOAT NOT NULL, 
	notes TEXT, 
	PRIMARY KEY (id), 
	FOREIGN KEY(printer_maintenance_id) REFERENCES printer_maintenance (id) ON DELETE CASCADE
);

CREATE TABLE maintenance_log_entries (
	id INTEGER NOT NULL, 
	printer_id INTEGER, 
	printer_name VARCHAR(255) NOT NULL, 
	maintenance_type_id INTEGER, 
	maintenance_type_name VARCHAR(100) NOT NULL, 
	hours_at_maintenance FLOAT NOT NULL, 
	notes TEXT, 
	performed_by_id INTEGER, 
	performed_by_username VARCHAR(100), 
	performed_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	history_id INTEGER, 
	PRIMARY KEY (id), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE SET NULL, 
	FOREIGN KEY(maintenance_type_id) REFERENCES maintenance_types (id) ON DELETE SET NULL, 
	FOREIGN KEY(performed_by_id) REFERENCES users (id) ON DELETE SET NULL, 
	FOREIGN KEY(history_id) REFERENCES maintenance_history (id) ON DELETE SET NULL
);

CREATE TABLE maintenance_types (
	id INTEGER NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	description TEXT, 
	default_interval_hours FLOAT NOT NULL, 
	interval_type VARCHAR(20) NOT NULL, 
	icon VARCHAR(50), 
	wiki_url VARCHAR(500), 
	is_system BOOLEAN NOT NULL, 
	is_deleted BOOLEAN NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE notification_digest_queue (
	id INTEGER NOT NULL, 
	provider_id INTEGER NOT NULL, 
	event_type VARCHAR(50) NOT NULL, 
	title VARCHAR(255) NOT NULL, 
	message TEXT NOT NULL, 
	printer_id INTEGER, 
	printer_name VARCHAR(100), 
	created_at DATETIME, 
	PRIMARY KEY (id), 
	FOREIGN KEY(provider_id) REFERENCES notification_providers (id) ON DELETE CASCADE, 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE SET NULL
);

CREATE TABLE notification_live_activities (
	id INTEGER NOT NULL, 
	provider_id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	print_key VARCHAR(200) NOT NULL, 
	job_key VARCHAR(160), 
	print_name VARCHAR(255) NOT NULL, 
	eta_seconds INTEGER, 
	eta_deadline DATETIME, 
	credential_key VARCHAR(64) NOT NULL, 
	activity_id VARCHAR(16), 
	state VARCHAR(20) NOT NULL, 
	end_reason VARCHAR(40), 
	failures INTEGER NOT NULL, 
	content TEXT NOT NULL, 
	expires_at DATETIME, 
	next_attempt_at DATETIME, 
	last_sent_at DATETIME, 
	created_at DATETIME NOT NULL, 
	PRIMARY KEY (id), 
	UNIQUE (provider_id, printer_id, print_key), 
	FOREIGN KEY(provider_id) REFERENCES notification_providers (id) ON DELETE CASCADE
);

CREATE TABLE notification_lock_screen_widgets (
	id INTEGER NOT NULL, 
	provider_id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	credential_key VARCHAR(64) NOT NULL, 
	widget_id VARCHAR(16), 
	state VARCHAR(20) NOT NULL, 
	content TEXT NOT NULL, 
	failures INTEGER NOT NULL, 
	next_attempt_at DATETIME, 
	last_sent_at DATETIME, 
	created_at DATETIME NOT NULL, 
	PRIMARY KEY (id), 
	UNIQUE (provider_id, printer_id), 
	FOREIGN KEY(provider_id) REFERENCES notification_providers (id) ON DELETE CASCADE
);

CREATE TABLE notification_logs (
	id INTEGER NOT NULL, 
	provider_id INTEGER NOT NULL, 
	event_type VARCHAR(50) NOT NULL, 
	title VARCHAR(255) NOT NULL, 
	message TEXT NOT NULL, 
	success BOOLEAN, 
	error_message TEXT, 
	printer_id INTEGER, 
	printer_name VARCHAR(100), 
	created_at DATETIME, 
	PRIMARY KEY (id), 
	FOREIGN KEY(provider_id) REFERENCES notification_providers (id) ON DELETE CASCADE, 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE SET NULL
);

CREATE TABLE notification_providers (
	id INTEGER NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	provider_type VARCHAR(50) NOT NULL, 
	enabled BOOLEAN, 
	config TEXT NOT NULL, 
	attach_photo BOOLEAN DEFAULT 1, 
	on_print_start BOOLEAN, 
	on_print_complete BOOLEAN, 
	on_print_failed BOOLEAN, 
	on_print_stopped BOOLEAN, 
	on_print_progress BOOLEAN, 
	on_print_missing_spool_assignment BOOLEAN, 
	on_billing_charge_failed BOOLEAN DEFAULT 1, 
	on_printer_offline BOOLEAN, 
	on_printer_error BOOLEAN, 
	on_ai_failure_detection BOOLEAN DEFAULT 0, 
	on_filament_low BOOLEAN, 
	on_maintenance_due BOOLEAN, 
	on_ams_humidity_high BOOLEAN, 
	on_ams_temperature_high BOOLEAN, 
	on_ams_drying_suspended BOOLEAN DEFAULT 1, 
	on_ams_ht_humidity_high BOOLEAN, 
	on_ams_ht_temperature_high BOOLEAN, 
	on_ha_sensor_alert BOOLEAN DEFAULT 0, 
	on_location_ha_sensor_alert BOOLEAN DEFAULT 0, 
	on_plate_not_empty BOOLEAN, 
	on_plate_clear_required BOOLEAN DEFAULT 0, 
	on_print_confirm_request BOOLEAN DEFAULT 1, 
	telegram_verdict_mode VARCHAR(16) DEFAULT 'buttons', 
	on_bed_cooled BOOLEAN, 
	on_first_layer_complete BOOLEAN, 
	on_print_pause_upcoming BOOLEAN DEFAULT 0, 
	on_app_message BOOLEAN DEFAULT 0, 
	on_stock_reorder_alert BOOLEAN DEFAULT 0, 
	on_stock_break_alert BOOLEAN DEFAULT 0, 
	on_queue_job_added BOOLEAN, 
	on_queue_job_assigned BOOLEAN, 
	on_queue_job_started BOOLEAN, 
	on_queue_job_waiting BOOLEAN, 
	on_queue_job_skipped BOOLEAN, 
	on_queue_job_failed BOOLEAN, 
	on_queue_completed BOOLEAN, 
	quiet_hours_enabled BOOLEAN, 
	quiet_hours_start VARCHAR(5), 
	quiet_hours_end VARCHAR(5), 
	daily_digest_enabled BOOLEAN, 
	daily_digest_time VARCHAR(5), 
	printer_id INTEGER, 
	last_success DATETIME, 
	last_error TEXT, 
	last_error_at DATETIME, 
	created_at DATETIME, 
	updated_at DATETIME, 
	PRIMARY KEY (id), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE notification_templates (
	id INTEGER NOT NULL, 
	event_type VARCHAR(50) NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	title_template TEXT NOT NULL, 
	body_template TEXT NOT NULL, 
	is_default BOOLEAN NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	UNIQUE (event_type)
);

CREATE TABLE oidc_providers (
	id INTEGER NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	issuer_url VARCHAR(500) NOT NULL, 
	client_id VARCHAR(255) NOT NULL, 
	client_secret VARCHAR(512) NOT NULL, 
	scopes VARCHAR(500) NOT NULL, 
	is_enabled BOOLEAN NOT NULL, 
	auto_create_users BOOLEAN NOT NULL, 
	auto_link_existing_accounts BOOLEAN NOT NULL, 
	email_claim VARCHAR(64) DEFAULT 'email' NOT NULL, 
	require_email_verified BOOLEAN DEFAULT 1 NOT NULL, 
	default_group_id INTEGER, 
	group_claim VARCHAR(64) DEFAULT 'groups' NOT NULL, 
	group_mapping JSON DEFAULT '{}' NOT NULL, 
	icon_url TEXT, 
	icon_data BLOB, 
	icon_content_type VARCHAR(20), 
	icon_etag VARCHAR(64), 
	is_autologin BOOLEAN DEFAULT '0' NOT NULL, 
	is_env_managed BOOLEAN DEFAULT '0' NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT ck_auto_link_requires_verified_email_claim CHECK (auto_link_existing_accounts = FALSE OR email_claim != 'email' OR require_email_verified = TRUE), 
	CONSTRAINT ck_oidc_icon_triplet_co_null CHECK ((icon_data IS NULL) = (icon_content_type IS NULL) AND (icon_content_type IS NULL) = (icon_etag IS NULL)), 
	UNIQUE (name), 
	FOREIGN KEY(default_group_id) REFERENCES groups (id) ON DELETE SET NULL
);

CREATE TABLE orca_base_profiles (
	id INTEGER NOT NULL, 
	name VARCHAR(300) NOT NULL, 
	profile_type VARCHAR(20) NOT NULL, 
	setting TEXT NOT NULL, 
	fetched_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE pending_uploads (
	id INTEGER NOT NULL, 
	filename VARCHAR(255) NOT NULL, 
	file_path VARCHAR(500) NOT NULL, 
	file_size INTEGER NOT NULL, 
	metadata_print_name VARCHAR(255), 
	source_ip VARCHAR(45), 
	status VARCHAR(20) NOT NULL, 
	tags TEXT, 
	notes TEXT, 
	project_id INTEGER, 
	archived_id INTEGER, 
	uploaded_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	archived_at DATETIME, 
	PRIMARY KEY (id), 
	FOREIGN KEY(project_id) REFERENCES projects (id) ON DELETE SET NULL, 
	FOREIGN KEY(archived_id) REFERENCES print_archives (id) ON DELETE SET NULL
);

CREATE TABLE pipeline_jobs (
	id INTEGER NOT NULL, 
	pipeline_run_id INTEGER NOT NULL, 
	copy_index INTEGER NOT NULL, 
	assigned_printer_id INTEGER, 
	queue_entry_id INTEGER, 
	status VARCHAR(20) NOT NULL, 
	error_message TEXT, 
	dispatched_at DATETIME, 
	completed_at DATETIME, 
	PRIMARY KEY (id), 
	FOREIGN KEY(pipeline_run_id) REFERENCES pipeline_runs (id) ON DELETE CASCADE, 
	FOREIGN KEY(assigned_printer_id) REFERENCES printers (id) ON DELETE SET NULL, 
	FOREIGN KEY(queue_entry_id) REFERENCES print_queue (id) ON DELETE SET NULL
);

CREATE TABLE pipeline_runs (
	id INTEGER NOT NULL, 
	pipeline_id INTEGER, 
	source_library_file_id INTEGER, 
	source_archive_id INTEGER, 
	parent_run_id INTEGER, 
	copies INTEGER NOT NULL, 
	status VARCHAR(20) NOT NULL, 
	slice_job_id INTEGER, 
	sliced_library_file_id INTEGER, 
	eligibility_overridden BOOLEAN DEFAULT '0' NOT NULL, 
	error_message TEXT, 
	created_by INTEGER, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	started_at DATETIME, 
	completed_at DATETIME, 
	PRIMARY KEY (id), 
	FOREIGN KEY(pipeline_id) REFERENCES slicer_pipelines (id) ON DELETE SET NULL, 
	FOREIGN KEY(source_library_file_id) REFERENCES library_files (id) ON DELETE SET NULL, 
	FOREIGN KEY(source_archive_id) REFERENCES print_archives (id) ON DELETE SET NULL, 
	FOREIGN KEY(parent_run_id) REFERENCES pipeline_runs (id) ON DELETE SET NULL, 
	FOREIGN KEY(sliced_library_file_id) REFERENCES library_files (id) ON DELETE SET NULL, 
	FOREIGN KEY(created_by) REFERENCES users (id) ON DELETE SET NULL
);

CREATE TABLE print_archives (
	id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, 
	printer_id INTEGER, 
	project_id INTEGER, 
	library_file_id INTEGER, 
	cost_center_id INTEGER, 
	filename VARCHAR(255) NOT NULL, 
	file_path VARCHAR(500) NOT NULL, 
	file_size INTEGER NOT NULL, 
	content_hash VARCHAR(64), 
	gcode_hash VARCHAR(64), 
	thumbnail_path VARCHAR(500), 
	timelapse_path VARCHAR(500), 
	timelapse_plate_id INTEGER, 
	bambuddy_forced_timelapse BOOLEAN DEFAULT '0' NOT NULL, 
	timelapse_baseline JSON, 
	timelapse_baseline_unread BOOLEAN DEFAULT 0 NOT NULL, 
	source_3mf_path VARCHAR(500), 
	f3d_path VARCHAR(500), 
	print_name VARCHAR(255), 
	print_time_seconds INTEGER, 
	filament_used_grams FLOAT, 
	filament_type VARCHAR(50), 
	filament_color VARCHAR(200), 
	layer_height FLOAT, 
	total_layers INTEGER, 
	nozzle_diameter FLOAT, 
	bed_temperature INTEGER, 
	bed_type VARCHAR(64), 
	nozzle_temperature INTEGER, 
	sliced_for_model VARCHAR(50), 
	status VARCHAR(20) NOT NULL, 
	started_at DATETIME, 
	completed_at DATETIME, 
	subtask_id VARCHAR(64), 
	billing_run_id VARCHAR(36), 
	plate_id INTEGER, 
	plate_estimates JSON, 
	extra_data JSON, 
	makerworld_url VARCHAR(500), 
	designer VARCHAR(255), 
	external_url VARCHAR(500), 
	is_favorite BOOLEAN NOT NULL, 
	wallet_charge_skipped BOOLEAN DEFAULT 0 NOT NULL, 
	tags TEXT, 
	notes TEXT, 
	cost FLOAT, 
	photos JSON, 
	failure_reason VARCHAR(100), 
	quantity INTEGER NOT NULL, 
	user_verdict VARCHAR(10), 
	user_verdict_source VARCHAR(16), 
	user_verdict_at DATETIME, 
	confirm_requested BOOLEAN DEFAULT '0', 
	confirm_token VARCHAR(64), 
	confirm_token_used_at DATETIME, 
	energy_kwh FLOAT, 
	energy_cost FLOAT, 
	wear_cost FLOAT, 
	energy_start_kwh FLOAT, 
	energy_start_at DATETIME, 
	energy_start_plug_id INTEGER, 
	energy_start_price FLOAT, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	deleted_at DATETIME, 
	created_by_id INTEGER, 
	FOREIGN KEY(printer_id) REFERENCES printers (id), 
	FOREIGN KEY(project_id) REFERENCES projects (id) ON DELETE SET NULL, 
	FOREIGN KEY(library_file_id) REFERENCES library_files (id) ON DELETE SET NULL, 
	FOREIGN KEY(cost_center_id) REFERENCES cost_centers (id) ON DELETE SET NULL, 
	FOREIGN KEY(created_by_id) REFERENCES users (id) ON DELETE SET NULL
);

CREATE TABLE print_batch_plates (
	id INTEGER NOT NULL, 
	batch_id INTEGER NOT NULL, 
	plate_id INTEGER, 
	plate_name VARCHAR(255), 
	quantity_target INTEGER NOT NULL, 
	sort_order INTEGER NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT uq_batch_plate UNIQUE (batch_id, plate_id), 
	FOREIGN KEY(batch_id) REFERENCES print_batches (id) ON DELETE CASCADE
);

CREATE TABLE print_batches (
	id INTEGER NOT NULL, 
	name VARCHAR(255) NOT NULL, 
	archive_id INTEGER, 
	library_file_id INTEGER, 
	quantity INTEGER NOT NULL, 
	status VARCHAR(20) NOT NULL, 
	project_id INTEGER, 
	due_date DATETIME, 
	notes TEXT, 
	external_source VARCHAR(32), 
	external_ref VARCHAR(255), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	completed_at DATETIME, 
	created_by_id INTEGER, 
	PRIMARY KEY (id), 
	FOREIGN KEY(archive_id) REFERENCES print_archives (id) ON DELETE SET NULL, 
	FOREIGN KEY(library_file_id) REFERENCES library_files (id) ON DELETE SET NULL, 
	FOREIGN KEY(project_id) REFERENCES projects (id) ON DELETE SET NULL, 
	FOREIGN KEY(created_by_id) REFERENCES users (id) ON DELETE SET NULL
);

CREATE TABLE print_log_entries (
	id INTEGER NOT NULL, 
	archive_id INTEGER, 
	queue_item_id INTEGER, 
	print_name VARCHAR(255), 
	plate_id INTEGER, 
	printer_name VARCHAR(255), 
	printer_id INTEGER, 
	status VARCHAR(20) NOT NULL, 
	started_at DATETIME, 
	completed_at DATETIME, 
	duration_seconds INTEGER, 
	filament_type VARCHAR(50), 
	filament_color VARCHAR(50), 
	filament_used_grams FLOAT, 
	cost FLOAT, 
	energy_kwh FLOAT, 
	energy_cost FLOAT, 
	wear_cost FLOAT, 
	duration_unmeasured BOOLEAN, 
	failure_reason VARCHAR(100), 
	user_verdict VARCHAR(10), 
	thumbnail_path VARCHAR(500), 
	created_by_id INTEGER, 
	created_by_username VARCHAR(100), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	subtask_id VARCHAR(64), 
	billing_run_id VARCHAR(36), 
	cost_center_id INTEGER, 
	wallet_charge_skipped BOOLEAN, 
	energy_start_kwh FLOAT, 
	energy_start_at DATETIME, 
	energy_start_plug_id INTEGER, 
	energy_start_price FLOAT, 
	user_verdict_source VARCHAR(16), 
	user_verdict_at DATETIME, 
	confirm_requested BOOLEAN, 
	confirm_token VARCHAR(64), 
	confirm_token_used_at DATETIME, 
	photos JSON, 
	timelapse_path VARCHAR(500), 
	timelapse_baseline JSON, 
	timelapse_baseline_unread BOOLEAN, 
	bambuddy_forced_timelapse BOOLEAN, 
	notes TEXT, 
	quantity INTEGER, 
	ended_offline BOOLEAN, 
	PRIMARY KEY (id), 
	FOREIGN KEY(archive_id) REFERENCES print_archives (id) ON DELETE SET NULL, 
	FOREIGN KEY(queue_item_id) REFERENCES print_queue (id) ON DELETE SET NULL, 
	FOREIGN KEY(created_by_id) REFERENCES users (id) ON DELETE SET NULL
);

CREATE TABLE print_queue (
	id INTEGER NOT NULL, 
	printer_id INTEGER, 
	target_model VARCHAR(50), 
	target_location VARCHAR(100), 
	required_filament_types TEXT, 
	waiting_reason TEXT, 
	archive_id INTEGER, 
	library_file_id INTEGER, 
	cost_center_id INTEGER, 
	estimated_cost FLOAT, 
	billing_run_id VARCHAR(36), 
	project_id INTEGER, 
	batch_id INTEGER, 
	position INTEGER NOT NULL, 
	scheduled_time DATETIME, 
	manual_start BOOLEAN NOT NULL, 
	require_previous_success BOOLEAN NOT NULL, 
	auto_off_after BOOLEAN NOT NULL, 
	ams_mapping TEXT, 
	filament_overrides TEXT, 
	plate_id INTEGER, 
	print_time_seconds INTEGER, 
	been_jumped BOOLEAN NOT NULL, 
	gcode_injection BOOLEAN NOT NULL, 
	dispatch_attempts INTEGER DEFAULT '0' NOT NULL, 
	nozzle_mapping TEXT, 
	nozzles_info TEXT, 
	nozzle_rack_choice TEXT, 
	color_substitutions TEXT, 
	cleanup_library_after_dispatch BOOLEAN DEFAULT 0 NOT NULL, 
	bed_levelling VARCHAR(8) DEFAULT 'auto' NOT NULL, 
	flow_cali VARCHAR(8) DEFAULT 'auto' NOT NULL, 
	vibration_cali BOOLEAN NOT NULL, 
	layer_inspect BOOLEAN NOT NULL, 
	timelapse BOOLEAN NOT NULL, 
	use_ams BOOLEAN NOT NULL, 
	nozzle_offset_cali VARCHAR(8) DEFAULT 'auto' NOT NULL, 
	confirm_outcome BOOLEAN DEFAULT '0' NOT NULL, 
	preheat_override VARCHAR(10) DEFAULT 'inherit' NOT NULL, 
	preheat_chamber_target_override INTEGER, 
	status VARCHAR(20) NOT NULL, 
	dispatching_at DATETIME, 
	gate_acknowledged BOOLEAN DEFAULT 0 NOT NULL, 
	filament_short BOOLEAN DEFAULT 0 NOT NULL, 
	skip_filament_check BOOLEAN DEFAULT 0 NOT NULL, 
	started_at DATETIME, 
	completed_at DATETIME, 
	error_message TEXT, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	created_by_id INTEGER, 
	PRIMARY KEY (id), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE, 
	FOREIGN KEY(archive_id) REFERENCES print_archives (id) ON DELETE CASCADE, 
	FOREIGN KEY(library_file_id) REFERENCES library_files (id) ON DELETE CASCADE, 
	FOREIGN KEY(cost_center_id) REFERENCES cost_centers (id) ON DELETE SET NULL, 
	FOREIGN KEY(project_id) REFERENCES projects (id) ON DELETE SET NULL, 
	FOREIGN KEY(batch_id) REFERENCES print_batches (id) ON DELETE SET NULL, 
	FOREIGN KEY(created_by_id) REFERENCES users (id) ON DELETE SET NULL
);

CREATE TABLE print_queue_variants (
	id INTEGER NOT NULL, 
	queue_item_id INTEGER NOT NULL, 
	position INTEGER NOT NULL, 
	library_file_id INTEGER NOT NULL, 
	target_model VARCHAR(50) NOT NULL, 
	plate_id INTEGER, 
	ams_mapping TEXT, 
	nozzle_mapping TEXT, 
	nozzle_rack_choice TEXT, 
	filament_overrides TEXT, 
	required_filament_types TEXT, 
	print_time_seconds INTEGER, 
	attempt_count INTEGER DEFAULT '0' NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(queue_item_id) REFERENCES print_queue (id) ON DELETE CASCADE, 
	FOREIGN KEY(library_file_id) REFERENCES library_files (id) ON DELETE CASCADE
);

CREATE TABLE printer_ha_sensors (
	id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	entity_id VARCHAR(255) NOT NULL, 
	kind VARCHAR(16) NOT NULL, 
	device_class VARCHAR(32), 
	unit VARCHAR(16), 
	alert_state VARCHAR(8), 
	alert_above FLOAT, 
	alert_below FLOAT, 
	block_print BOOLEAN DEFAULT '0' NOT NULL, 
	notify_on_alert BOOLEAN DEFAULT '0' NOT NULL, 
	show_on_printer_card BOOLEAN DEFAULT '1' NOT NULL, 
	sort_order INTEGER DEFAULT '0' NOT NULL, 
	last_state VARCHAR(64), 
	last_changed DATETIME, 
	last_checked DATETIME, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE printer_locations (
	id INTEGER NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	icon VARCHAR(50), 
	color VARCHAR(7), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	UNIQUE (name)
);

CREATE TABLE printer_maintenance (
	id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	maintenance_type_id INTEGER NOT NULL, 
	custom_interval_hours FLOAT, 
	custom_interval_type VARCHAR(20), 
	enabled BOOLEAN NOT NULL, 
	last_performed_at DATETIME, 
	last_performed_hours FLOAT NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE, 
	FOREIGN KEY(maintenance_type_id) REFERENCES maintenance_types (id) ON DELETE CASCADE
);

CREATE TABLE printer_sensor_history (
	id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	sensor_kind VARCHAR(32) NOT NULL, 
	value FLOAT, 
	target FLOAT, 
	recorded_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE printers (
	id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, 
	name VARCHAR(100) NOT NULL, 
	serial_number VARCHAR(50) NOT NULL, 
	ip_address VARCHAR(253) NOT NULL, 
	access_code VARCHAR(20) NOT NULL, 
	model VARCHAR(50), 
	location VARCHAR(100), 
	nozzle_count INTEGER NOT NULL, 
	is_active BOOLEAN NOT NULL, 
	auto_archive BOOLEAN NOT NULL, 
	print_hours_offset FLOAT NOT NULL, 
	wear_cost_per_hour FLOAT, 
	dispatch_priority INTEGER, 
	runtime_seconds INTEGER NOT NULL, 
	last_runtime_update DATETIME, 
	external_camera_url VARCHAR(500), 
	external_camera_type VARCHAR(20), 
	external_camera_enabled BOOLEAN NOT NULL, 
	external_camera_snapshot_url VARCHAR(500), 
	camera_rotation INTEGER NOT NULL, 
	camera_light_auto BOOLEAN DEFAULT 0 NOT NULL, 
	plate_detection_enabled BOOLEAN NOT NULL, 
	plate_detection_roi_x FLOAT, 
	plate_detection_roi_y FLOAT, 
	plate_detection_roi_w FLOAT, 
	plate_detection_roi_h FLOAT, 
	awaiting_plate_clear BOOLEAN NOT NULL, 
	wled_config JSON, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	UNIQUE (serial_number)
);

CREATE TABLE project_bom_items (
	id INTEGER NOT NULL, 
	project_id INTEGER NOT NULL, 
	name VARCHAR(255) NOT NULL, 
	quantity_needed INTEGER NOT NULL, 
	quantity_acquired INTEGER NOT NULL, 
	unit_price FLOAT, 
	sourcing_url VARCHAR(512), 
	archive_id INTEGER, 
	stl_filename VARCHAR(255), 
	remarks TEXT, 
	sort_order INTEGER NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(project_id) REFERENCES projects (id) ON DELETE CASCADE, 
	FOREIGN KEY(archive_id) REFERENCES print_archives (id) ON DELETE SET NULL
);

CREATE TABLE projects (
	id INTEGER NOT NULL, 
	name VARCHAR(255) NOT NULL, 
	description TEXT, 
	color VARCHAR(20), 
	status VARCHAR(20) NOT NULL, 
	url VARCHAR(2048), 
	cover_image_filename VARCHAR(255), 
	target_count INTEGER, 
	target_parts_count INTEGER, 
	target_sets INTEGER, 
	notes TEXT, 
	attachments JSON, 
	tags TEXT, 
	due_date DATETIME, 
	priority VARCHAR(20) NOT NULL, 
	budget FLOAT, 
	is_template BOOLEAN NOT NULL, 
	template_source_id INTEGER, 
	parent_id INTEGER, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(parent_id) REFERENCES projects (id)
);

CREATE TABLE scheduled_dryings (
	id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	ams_id INTEGER NOT NULL, 
	"temp" INTEGER NOT NULL, 
	duration_hours INTEGER NOT NULL, 
	filament VARCHAR(50) NOT NULL, 
	rotate_tray BOOLEAN NOT NULL, 
	start_after DATETIME, 
	status VARCHAR(20) NOT NULL, 
	waiting_reason TEXT, 
	error_message TEXT, 
	error_code VARCHAR(32), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	started_at DATETIME, 
	completed_at DATETIME, 
	created_by_id INTEGER, 
	PRIMARY KEY (id), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE, 
	FOREIGN KEY(created_by_id) REFERENCES users (id) ON DELETE SET NULL
);

CREATE TABLE settings (
	id INTEGER NOT NULL, 
	"key" VARCHAR(100) NOT NULL, 
	value TEXT NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE slicer_pipelines (
	id INTEGER NOT NULL, 
	name VARCHAR(200) NOT NULL, 
	description VARCHAR(1000), 
	printer_preset_source VARCHAR(20) NOT NULL, 
	printer_preset_id VARCHAR(200) NOT NULL, 
	process_preset_source VARCHAR(20) NOT NULL, 
	process_preset_id VARCHAR(200) NOT NULL, 
	filament_presets_json TEXT NOT NULL, 
	bed_type VARCHAR(64), 
	target_kind VARCHAR(20) NOT NULL, 
	target_printer_id INTEGER, 
	target_model_class VARCHAR(20), 
	fanout_strategy VARCHAR(20) NOT NULL, 
	created_by INTEGER, 
	is_deleted BOOLEAN DEFAULT '0' NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(target_printer_id) REFERENCES printers (id) ON DELETE SET NULL, 
	FOREIGN KEY(created_by) REFERENCES users (id) ON DELETE SET NULL
);

CREATE TABLE slot_preset_mappings (
	id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	ams_id INTEGER NOT NULL, 
	tray_id INTEGER NOT NULL, 
	preset_id VARCHAR(100) NOT NULL, 
	preset_name VARCHAR(200) NOT NULL, 
	preset_source VARCHAR(20) NOT NULL, 
	tray_info_idx VARCHAR(32), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT uq_slot_preset UNIQUE (printer_id, ams_id, tray_id), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE smart_plug_energy_snapshots (
	id INTEGER NOT NULL, 
	plug_id INTEGER NOT NULL, 
	recorded_at DATETIME NOT NULL, 
	lifetime_kwh FLOAT NOT NULL, 
	price_per_kwh FLOAT, 
	kwh_to_date FLOAT, 
	cost_to_date FLOAT, 
	PRIMARY KEY (id), 
	FOREIGN KEY(plug_id) REFERENCES smart_plugs (id) ON DELETE CASCADE
);

CREATE TABLE smart_plugs (
	id INTEGER NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	ip_address VARCHAR(45), 
	plug_type VARCHAR(20) NOT NULL, 
	ha_entity_id VARCHAR(100), 
	ha_power_entity VARCHAR(100), 
	ha_energy_today_entity VARCHAR(100), 
	ha_energy_total_entity VARCHAR(100), 
	mqtt_topic VARCHAR(200), 
	mqtt_power_topic VARCHAR(200), 
	mqtt_power_path VARCHAR(100), 
	mqtt_power_multiplier FLOAT NOT NULL, 
	mqtt_energy_topic VARCHAR(200), 
	mqtt_energy_path VARCHAR(100), 
	mqtt_energy_multiplier FLOAT NOT NULL, 
	mqtt_energy_total_path VARCHAR(100), 
	mqtt_energy_total_multiplier FLOAT DEFAULT '1.0' NOT NULL, 
	mqtt_state_topic VARCHAR(200), 
	mqtt_state_path VARCHAR(100), 
	mqtt_state_on_value VARCHAR(50), 
	mqtt_multiplier FLOAT NOT NULL, 
	rest_on_url VARCHAR(500), 
	rest_on_body TEXT, 
	rest_off_url VARCHAR(500), 
	rest_off_body TEXT, 
	rest_method VARCHAR(10), 
	rest_headers TEXT, 
	rest_status_url VARCHAR(500), 
	rest_status_path VARCHAR(200), 
	rest_status_on_value VARCHAR(50), 
	rest_power_url VARCHAR(500), 
	rest_power_path VARCHAR(200), 
	rest_power_multiplier FLOAT DEFAULT '1.0' NOT NULL, 
	rest_energy_url VARCHAR(500), 
	rest_energy_path VARCHAR(200), 
	rest_energy_multiplier FLOAT DEFAULT '1.0' NOT NULL, 
	rest_energy_total_path VARCHAR(200), 
	rest_energy_total_multiplier FLOAT DEFAULT '1.0' NOT NULL, 
	printer_id INTEGER, 
	controls_printer_power BOOLEAN DEFAULT '1' NOT NULL, 
	enabled BOOLEAN NOT NULL, 
	auto_on BOOLEAN NOT NULL, 
	auto_off BOOLEAN NOT NULL, 
	auto_off_persistent BOOLEAN NOT NULL, 
	off_delay_mode VARCHAR(20) NOT NULL, 
	off_delay_minutes INTEGER NOT NULL, 
	off_temp_threshold INTEGER NOT NULL, 
	auto_off_after_drying BOOLEAN DEFAULT '0' NOT NULL, 
	off_delay_after_drying_minutes INTEGER DEFAULT '10' NOT NULL, 
	username VARCHAR(50), 
	password VARCHAR(100), 
	power_alert_enabled BOOLEAN NOT NULL, 
	power_alert_high FLOAT, 
	power_alert_low FLOAT, 
	power_alert_last_triggered DATETIME, 
	schedule_enabled BOOLEAN NOT NULL, 
	schedule_on_time VARCHAR(5), 
	schedule_off_time VARCHAR(5), 
	show_in_switchbar BOOLEAN NOT NULL, 
	show_on_printer_card BOOLEAN NOT NULL, 
	last_state VARCHAR(10), 
	last_checked DATETIME, 
	auto_off_executed BOOLEAN NOT NULL, 
	auto_off_pending BOOLEAN NOT NULL, 
	auto_off_pending_since DATETIME, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE sponsor_toast_state (
	id INTEGER NOT NULL, 
	user_id INTEGER, 
	last_shown_at DATETIME, 
	milestones_seen TEXT NOT NULL, 
	last_seen_version VARCHAR(50), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT uq_sponsor_toast_state_user_id UNIQUE (user_id), 
	FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);

CREATE TABLE spool (
	id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, 
	material VARCHAR(50) NOT NULL, 
	subtype VARCHAR(50), 
	color_name VARCHAR(100), 
	rgba VARCHAR(8), 
	extra_colors VARCHAR(255), 
	effect_type VARCHAR(20), 
	brand VARCHAR(100), 
	label_weight INTEGER NOT NULL, 
	core_weight INTEGER NOT NULL, 
	core_weight_catalog_id INTEGER, 
	weight_used FLOAT NOT NULL, 
	weight_used_baseline FLOAT DEFAULT '0' NOT NULL, 
	weight_locked BOOLEAN NOT NULL, 
	last_scale_weight INTEGER, 
	last_weighed_at DATETIME, 
	slicer_filament VARCHAR(50), 
	slicer_filament_name VARCHAR(100), 
	nozzle_temp_min INTEGER, 
	nozzle_temp_max INTEGER, 
	note VARCHAR(500), 
	added_full BOOLEAN, 
	category VARCHAR(50), 
	low_stock_threshold_pct INTEGER, 
	material_number VARCHAR(64), 
	cost_per_kg FLOAT, 
	storage_location VARCHAR(255), 
	location_id INTEGER, 
	last_used DATETIME, 
	last_dried_at DATETIME, 
	last_dried_temp INTEGER, 
	last_dried_hours FLOAT, 
	encode_time DATETIME, 
	tag_uid VARCHAR(32), 
	tray_uuid VARCHAR(32), 
	data_origin VARCHAR(20), 
	tag_type VARCHAR(20), 
	archived_at DATETIME, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	FOREIGN KEY(location_id) REFERENCES locations (id)
);

CREATE TABLE spool_assignment (
	id INTEGER NOT NULL, 
	spool_id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	ams_id INTEGER NOT NULL, 
	tray_id INTEGER NOT NULL, 
	fingerprint_color VARCHAR(8), 
	fingerprint_type VARCHAR(50), 
	assigned_by_user_id INTEGER, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	UNIQUE (printer_id, ams_id, tray_id), 
	FOREIGN KEY(spool_id) REFERENCES spool (id) ON DELETE CASCADE, 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE, 
	FOREIGN KEY(assigned_by_user_id) REFERENCES users (id) ON DELETE SET NULL
);

CREATE TABLE spool_catalog (
	id INTEGER NOT NULL, 
	name VARCHAR(200) NOT NULL, 
	weight INTEGER NOT NULL, 
	is_default BOOLEAN NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE spool_filament_preset (
	id INTEGER NOT NULL, 
	spool_id INTEGER NOT NULL, 
	printer_model VARCHAR(50) NOT NULL, 
	nozzle_diameter VARCHAR(10) NOT NULL, 
	slicer_filament VARCHAR(128), 
	slicer_filament_name VARCHAR(255), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	UNIQUE (spool_id, printer_model, nozzle_diameter), 
	FOREIGN KEY(spool_id) REFERENCES spool (id) ON DELETE CASCADE
);

CREATE TABLE spool_k_profile (
	id INTEGER NOT NULL, 
	spool_id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	extruder INTEGER NOT NULL, 
	nozzle_diameter VARCHAR(10) NOT NULL, 
	nozzle_type VARCHAR(50), 
	k_value FLOAT NOT NULL, 
	name VARCHAR(100), 
	cali_idx INTEGER, 
	setting_id VARCHAR(50), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(spool_id) REFERENCES spool (id) ON DELETE CASCADE, 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE spool_suppliers (
	id INTEGER NOT NULL, 
	spool_id INTEGER NOT NULL, 
	supplier_id INTEGER NOT NULL, 
	supplier_article_number VARCHAR(100), 
	quoted_price_per_kg FLOAT, 
	is_purchase_source BOOLEAN NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT uq_spool_suppliers_spool_supplier UNIQUE (spool_id, supplier_id), 
	FOREIGN KEY(spool_id) REFERENCES spool (id) ON DELETE CASCADE, 
	FOREIGN KEY(supplier_id) REFERENCES suppliers (id)
);

CREATE TABLE spool_usage_history (
	id INTEGER NOT NULL, 
	spool_id INTEGER, 
	spoolman_spool_id INTEGER, 
	printer_id INTEGER, 
	print_name VARCHAR(500), 
	archive_id INTEGER, 
	weight_used FLOAT NOT NULL, 
	percent_used INTEGER NOT NULL, 
	status VARCHAR(20) NOT NULL, 
	cost FLOAT, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(spool_id) REFERENCES spool (id) ON DELETE SET NULL, 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE SET NULL, 
	FOREIGN KEY(archive_id) REFERENCES print_archives (id) ON DELETE SET NULL
);

CREATE TABLE spoolbuddy_devices (
	id INTEGER NOT NULL, 
	device_id VARCHAR(50) NOT NULL, 
	hostname VARCHAR(100) NOT NULL, 
	ip_address VARCHAR(45) NOT NULL, 
	firmware_version VARCHAR(20), 
	has_nfc BOOLEAN NOT NULL, 
	has_scale BOOLEAN NOT NULL, 
	tare_offset INTEGER NOT NULL, 
	calibration_factor FLOAT NOT NULL, 
	nfc_reader_type VARCHAR(20), 
	nfc_connection VARCHAR(20), 
	backend_url VARCHAR(255), 
	display_brightness INTEGER NOT NULL, 
	display_blank_timeout INTEGER NOT NULL, 
	has_backlight BOOLEAN NOT NULL, 
	last_calibrated_at DATETIME, 
	last_seen DATETIME, 
	pending_command VARCHAR(50), 
	pending_write_payload TEXT, 
	update_status VARCHAR(20), 
	update_message VARCHAR(255), 
	pending_system_payload TEXT, 
	nfc_ok BOOLEAN NOT NULL, 
	scale_ok BOOLEAN NOT NULL, 
	uptime_s INTEGER NOT NULL, 
	system_stats TEXT, 
	ssh_host_key TEXT, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE spoolman_filament_preset (
	id INTEGER NOT NULL, 
	spoolman_spool_id INTEGER NOT NULL, 
	printer_model VARCHAR(50) NOT NULL, 
	nozzle_diameter VARCHAR(10) NOT NULL, 
	slicer_filament VARCHAR(128), 
	slicer_filament_name VARCHAR(255), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	UNIQUE (spoolman_spool_id, printer_model, nozzle_diameter)
);

CREATE TABLE spoolman_k_profile (
	id INTEGER NOT NULL, 
	spoolman_spool_id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	extruder INTEGER NOT NULL, 
	nozzle_diameter VARCHAR(10) NOT NULL, 
	nozzle_type VARCHAR(50), 
	k_value FLOAT NOT NULL, 
	name VARCHAR(100), 
	cali_idx INTEGER, 
	setting_id VARCHAR(50), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	UNIQUE (spoolman_spool_id, printer_id, extruder, nozzle_diameter), 
	CONSTRAINT ck_extruder_range CHECK (extruder >= 0 AND extruder <= 1), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE spoolman_slot_assignments (
	id INTEGER NOT NULL, 
	printer_id INTEGER NOT NULL, 
	ams_id INTEGER NOT NULL, 
	tray_id INTEGER NOT NULL, 
	spoolman_spool_id INTEGER NOT NULL, 
	assigned_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT uq_slot_assignment UNIQUE (printer_id, ams_id, tray_id), 
	CONSTRAINT ck_ams_id_range CHECK ((ams_id >= 0 AND ams_id <= 7) OR ams_id = 32 OR (ams_id >= 128 AND ams_id <= 191) OR ams_id = 255), 
	CONSTRAINT ck_tray_id_range CHECK (tray_id >= 0 AND (tray_id <= 3 OR (ams_id IN (32, 255) AND tray_id <= 8))), 
	FOREIGN KEY(printer_id) REFERENCES printers (id) ON DELETE CASCADE
);

CREATE TABLE spoolman_spool_suppliers (
	id INTEGER NOT NULL, 
	spoolman_spool_id INTEGER NOT NULL, 
	supplier_id INTEGER NOT NULL, 
	supplier_article_number VARCHAR(100), 
	quoted_price_per_kg FLOAT, 
	is_purchase_source BOOLEAN NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT uq_spoolman_spool_suppliers_pair UNIQUE (spoolman_spool_id, supplier_id), 
	FOREIGN KEY(supplier_id) REFERENCES suppliers (id)
);

CREATE TABLE suppliers (
	id INTEGER NOT NULL, 
	name VARCHAR(200) NOT NULL, 
	name_key VARCHAR(200) NOT NULL, 
	website VARCHAR(500), 
	customer_number VARCHAR(100), 
	note VARCHAR(500), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id)
);

CREATE TABLE telegram_pending_verdicts (
	id INTEGER NOT NULL, 
	provider_id INTEGER NOT NULL, 
	chat_id VARCHAR(64) NOT NULL, 
	message_id INTEGER NOT NULL, 
	archive_id INTEGER NOT NULL, 
	confirm_token VARCHAR(64), 
	has_caption BOOLEAN, 
	message_text TEXT, 
	created_at DATETIME, 
	PRIMARY KEY (id), 
	FOREIGN KEY(provider_id) REFERENCES notification_providers (id) ON DELETE CASCADE, 
	FOREIGN KEY(archive_id) REFERENCES print_archives (id) ON DELETE CASCADE
);

CREATE TABLE user_email_preferences (
	id INTEGER NOT NULL, 
	user_id INTEGER NOT NULL, 
	notify_print_start BOOLEAN NOT NULL, 
	notify_print_complete BOOLEAN NOT NULL, 
	notify_print_failed BOOLEAN NOT NULL, 
	notify_print_stopped BOOLEAN NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);

CREATE TABLE user_groups (
	user_id INTEGER NOT NULL, 
	group_id INTEGER NOT NULL, 
	PRIMARY KEY (user_id, group_id), 
	FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE, 
	FOREIGN KEY(group_id) REFERENCES groups (id) ON DELETE CASCADE
);

CREATE TABLE user_oidc_links (
	id INTEGER NOT NULL, 
	user_id INTEGER NOT NULL, 
	provider_id INTEGER NOT NULL, 
	provider_user_id VARCHAR(500) NOT NULL, 
	provider_email VARCHAR(255), 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT uq_oidc_link_provider_sub UNIQUE (provider_id, provider_user_id), 
	CONSTRAINT uq_oidc_link_user_provider UNIQUE (user_id, provider_id), 
	FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE, 
	FOREIGN KEY(provider_id) REFERENCES oidc_providers (id) ON DELETE CASCADE
);

CREATE TABLE user_otp_codes (
	id INTEGER NOT NULL, 
	user_id INTEGER NOT NULL, 
	code_hash VARCHAR(255) NOT NULL, 
	attempts INTEGER NOT NULL, 
	used BOOLEAN NOT NULL, 
	expires_at DATETIME NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);

CREATE TABLE user_totp (
	id INTEGER NOT NULL, 
	user_id INTEGER NOT NULL, 
	secret VARCHAR(512) NOT NULL, 
	is_enabled BOOLEAN NOT NULL, 
	backup_codes_json TEXT, 
	last_totp_counter BIGINT, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);

CREATE TABLE user_wallets (
	id INTEGER NOT NULL, 
	user_id INTEGER NOT NULL, 
	balance NUMERIC(14, 2) NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE CASCADE
);

CREATE TABLE users (
	id INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT, 
	username VARCHAR(100) NOT NULL, 
	email VARCHAR(255), 
	password_hash VARCHAR(255), 
	role VARCHAR(20) NOT NULL, 
	auth_source VARCHAR(20) NOT NULL, 
	is_active BOOLEAN NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	password_changed_at DATETIME, 
	cloud_token VARCHAR(500), 
	cloud_email VARCHAR(255), 
	cloud_region VARCHAR(10), 
	cloud_token_invalid_at DATETIME, 
	orca_cloud_token VARCHAR(2000), 
	orca_cloud_refresh_token VARCHAR(128), 
	orca_cloud_expires_at DATETIME, 
	orca_cloud_email VARCHAR(255), 
	orca_cloud_user_id VARCHAR(64), 
	orca_cloud_pending_verifier VARCHAR(64), 
	orca_cloud_pending_state VARCHAR(32), 
	orca_cloud_pending_at DATETIME
);

CREATE TABLE virtual_printers (
	id INTEGER NOT NULL, 
	name VARCHAR(100) NOT NULL, 
	enabled BOOLEAN NOT NULL, 
	mode VARCHAR(20) NOT NULL, 
	auto_dispatch BOOLEAN DEFAULT 'true' NOT NULL, 
	queue_force_color_match BOOLEAN DEFAULT 'false' NOT NULL, 
	save_ams_mapping BOOLEAN DEFAULT 'false' NOT NULL, 
	gcode_injection BOOLEAN DEFAULT 'false' NOT NULL, 
	model VARCHAR(50), 
	access_code VARCHAR(8), 
	target_printer_id INTEGER, 
	bind_ip VARCHAR(45), 
	remote_interface_ip VARCHAR(45), 
	tailscale_disabled BOOLEAN DEFAULT 'true' NOT NULL, 
	serial_suffix VARCHAR(9) NOT NULL, 
	position INTEGER NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	updated_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	FOREIGN KEY(target_printer_id) REFERENCES printers (id) ON DELETE SET NULL
);

CREATE TABLE wallet_transactions (
	id INTEGER NOT NULL, 
	user_id INTEGER, 
	cost_center_id INTEGER, 
	transaction_type VARCHAR(40) NOT NULL, 
	amount NUMERIC(14, 2) NOT NULL, 
	balance_after NUMERIC(14, 2), 
	description TEXT, 
	created_by_user_id INTEGER, 
	print_run_id VARCHAR(100), 
	print_archive_id INTEGER, 
	print_queue_id INTEGER, 
	is_voided BOOLEAN NOT NULL, 
	created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, 
	PRIMARY KEY (id), 
	CONSTRAINT ck_wallet_transactions_transaction_type CHECK (transaction_type IN ('print_charge', 'deposit', 'withdraw', 'manual_adjustment')), 
	FOREIGN KEY(user_id) REFERENCES users (id) ON DELETE SET NULL, 
	FOREIGN KEY(cost_center_id) REFERENCES cost_centers (id) ON DELETE SET NULL, 
	FOREIGN KEY(created_by_user_id) REFERENCES users (id) ON DELETE SET NULL, 
	FOREIGN KEY(print_archive_id) REFERENCES print_archives (id) ON DELETE SET NULL, 
	FOREIGN KEY(print_queue_id) REFERENCES print_queue (id) ON DELETE SET NULL
);

CREATE INDEX ix_ams_history_printer_ams_time ON ams_sensor_history (printer_id, ams_id, recorded_at);

CREATE INDEX ix_ams_sensor_history_recorded_at ON ams_sensor_history (recorded_at);

CREATE INDEX ix_announcement_reads_announcement_id ON announcement_reads (announcement_id);

CREATE INDEX ix_announcement_reads_user_id ON announcement_reads (user_id);

CREATE UNIQUE INDEX ix_announcements_public_id ON announcements (public_id);

CREATE INDEX ix_api_keys_user_id ON api_keys (user_id);

CREATE INDEX ix_archive_aliases_archive_id ON archive_aliases (archive_id);

CREATE UNIQUE INDEX ix_auth_ephemeral_tokens_token ON auth_ephemeral_tokens (token);

CREATE INDEX ix_auth_rate_limit_events_username ON auth_rate_limit_events (username);

CREATE INDEX ix_budget_reservations_cost_center_id ON budget_reservations (cost_center_id);

CREATE INDEX ix_budget_reservations_print_archive_id ON budget_reservations (print_archive_id);

CREATE INDEX ix_budget_reservations_source_id ON budget_reservations (source_id);

CREATE INDEX ix_budget_reservations_source_type ON budget_reservations (source_type);

CREATE INDEX ix_budget_reservations_status ON budget_reservations (status);

CREATE INDEX ix_connected_app_grants_app_id ON connected_app_grants (app_id);

CREATE INDEX ix_connected_app_grants_user_id ON connected_app_grants (user_id);

CREATE UNIQUE INDEX ix_connected_apps_client_id ON connected_apps (client_id);

CREATE INDEX ix_cost_center_members_cost_center_id ON cost_center_members (cost_center_id);

CREATE INDEX ix_cost_center_members_user_id ON cost_center_members (user_id);

CREATE UNIQUE INDEX ix_cost_centers_code ON cost_centers (code);

CREATE INDEX ix_cost_centers_name ON cost_centers (name);

CREATE INDEX ix_filament_profile_default_k_profile_printer_id ON filament_profile_default_k_profile (printer_id);

CREATE INDEX ix_filament_profile_default_k_profile_profile_default_id ON filament_profile_default_k_profile (profile_default_id);

CREATE INDEX ix_filament_profile_default_preset_profile_default_id ON filament_profile_default_preset (profile_default_id);

CREATE UNIQUE INDEX ix_groups_name ON groups (name);

CREATE INDEX ix_hms_notified_faults_printer_id ON hms_notified_faults (printer_id);

CREATE UNIQUE INDEX ix_kprofile_notes_printer_setting ON kprofile_notes (printer_id, setting_id);

CREATE UNIQUE INDEX ix_label_templates_name_key ON label_templates (name_key);

CREATE INDEX ix_library_files_deleted_at ON library_files (deleted_at);

CREATE INDEX ix_library_files_source_url ON library_files (source_url);

CREATE INDEX ix_library_files_variant_group_id ON library_files (variant_group_id);

CREATE UNIQUE INDEX ix_library_tags_name_key ON library_tags (name_key);

CREATE INDEX ix_location_ha_sensors_location_id ON location_ha_sensors (location_id);

CREATE UNIQUE INDEX ix_locations_name_key ON locations (name_key);

CREATE INDEX ix_long_lived_tokens_lookup_prefix ON long_lived_tokens (lookup_prefix);

CREATE INDEX ix_long_lived_tokens_user_id ON long_lived_tokens (user_id);

CREATE INDEX ix_maintenance_log_entries_history_id ON maintenance_log_entries (history_id);

CREATE INDEX ix_maintenance_log_entries_maintenance_type_id ON maintenance_log_entries (maintenance_type_id);

CREATE INDEX ix_maintenance_log_entries_performed_at ON maintenance_log_entries (performed_at);

CREATE INDEX ix_maintenance_log_entries_printer_id ON maintenance_log_entries (printer_id);

CREATE INDEX ix_notification_digest_queue_created_at ON notification_digest_queue (created_at);

CREATE INDEX ix_notification_digest_queue_id ON notification_digest_queue (id);

CREATE INDEX ix_notification_live_activities_printer_id ON notification_live_activities (printer_id);

CREATE INDEX ix_notification_live_activities_provider_id ON notification_live_activities (provider_id);

CREATE INDEX ix_notification_lock_screen_widgets_printer_id ON notification_lock_screen_widgets (printer_id);

CREATE INDEX ix_notification_lock_screen_widgets_provider_id ON notification_lock_screen_widgets (provider_id);

CREATE INDEX ix_notification_logs_created_at ON notification_logs (created_at);

CREATE INDEX ix_notification_logs_id ON notification_logs (id);

CREATE INDEX ix_notification_providers_id ON notification_providers (id);

CREATE INDEX ix_notification_templates_id ON notification_templates (id);

CREATE UNIQUE INDEX ix_orca_base_profiles_name ON orca_base_profiles (name);

CREATE INDEX ix_plug_energy_snapshots_plug_time ON smart_plug_energy_snapshots (plug_id, recorded_at);

CREATE UNIQUE INDEX ix_print_archives_confirm_token ON print_archives (confirm_token);

CREATE INDEX ix_print_archives_deleted_at ON print_archives (deleted_at);

CREATE INDEX ix_print_archives_gcode_hash ON print_archives (gcode_hash);

CREATE INDEX ix_print_batch_plates_batch_id ON print_batch_plates (batch_id);

CREATE INDEX ix_print_log_entries_archive_id ON print_log_entries (archive_id);

CREATE UNIQUE INDEX ix_print_log_entries_confirm_token ON print_log_entries (confirm_token);

CREATE INDEX ix_print_log_entries_queue_item_id ON print_log_entries (queue_item_id);

CREATE INDEX ix_print_queue_variants_queue_item_id ON print_queue_variants (queue_item_id);

CREATE INDEX ix_printer_ha_sensors_printer_id ON printer_ha_sensors (printer_id);

CREATE INDEX ix_printer_sensor_history_printer_kind_time ON printer_sensor_history (printer_id, sensor_kind, recorded_at);

CREATE INDEX ix_printer_sensor_history_recorded_at ON printer_sensor_history (recorded_at);

CREATE UNIQUE INDEX ix_settings_key ON settings ("key");

CREATE INDEX ix_slot_assignment_spool ON spoolman_slot_assignments (spoolman_spool_id);

CREATE INDEX ix_sponsor_toast_state_user_id ON sponsor_toast_state (user_id);

CREATE INDEX ix_spool_filament_preset_spool_id ON spool_filament_preset (spool_id);

CREATE INDEX ix_spool_location_id ON spool (location_id);

CREATE INDEX ix_spool_suppliers_spool_id ON spool_suppliers (spool_id);

CREATE INDEX ix_spool_suppliers_supplier_id ON spool_suppliers (supplier_id);

CREATE UNIQUE INDEX ix_spoolbuddy_devices_device_id ON spoolbuddy_devices (device_id);

CREATE INDEX ix_spoolman_filament_preset_spoolman_spool_id ON spoolman_filament_preset (spoolman_spool_id);

CREATE INDEX ix_spoolman_k_profile_spool ON spoolman_k_profile (spoolman_spool_id);

CREATE INDEX ix_spoolman_spool_suppliers_spoolman_spool_id ON spoolman_spool_suppliers (spoolman_spool_id);

CREATE INDEX ix_spoolman_spool_suppliers_supplier_id ON spoolman_spool_suppliers (supplier_id);

CREATE INDEX ix_suppliers_name ON suppliers (name);

CREATE UNIQUE INDEX ix_suppliers_name_key ON suppliers (name_key);

CREATE INDEX ix_telegram_pending_verdicts_created_at ON telegram_pending_verdicts (created_at);

CREATE INDEX ix_telegram_pending_verdicts_id ON telegram_pending_verdicts (id);

CREATE INDEX ix_user_email_preferences_id ON user_email_preferences (id);

CREATE UNIQUE INDEX ix_user_email_preferences_user_id ON user_email_preferences (user_id);

CREATE INDEX ix_user_oidc_links_provider_id ON user_oidc_links (provider_id);

CREATE INDEX ix_user_oidc_links_user_id ON user_oidc_links (user_id);

CREATE INDEX ix_user_otp_codes_user_id ON user_otp_codes (user_id);

CREATE UNIQUE INDEX ix_user_totp_user_id ON user_totp (user_id);

CREATE UNIQUE INDEX ix_user_wallets_user_id ON user_wallets (user_id);

CREATE UNIQUE INDEX ix_users_email ON users (email);

CREATE UNIQUE INDEX ix_users_username ON users (username);

CREATE INDEX ix_wallet_transactions_cost_center_id ON wallet_transactions (cost_center_id);

CREATE INDEX ix_wallet_transactions_created_at ON wallet_transactions (created_at);

CREATE INDEX ix_wallet_transactions_created_by_user_id ON wallet_transactions (created_by_user_id);

CREATE INDEX ix_wallet_transactions_is_voided ON wallet_transactions (is_voided);

CREATE INDEX ix_wallet_transactions_print_archive_id ON wallet_transactions (print_archive_id);

CREATE INDEX ix_wallet_transactions_print_queue_id ON wallet_transactions (print_queue_id);

CREATE INDEX ix_wallet_transactions_print_run_id ON wallet_transactions (print_run_id);

CREATE INDEX ix_wallet_transactions_transaction_type ON wallet_transactions (transaction_type);

CREATE INDEX ix_wallet_transactions_user_id ON wallet_transactions (user_id);

CREATE UNIQUE INDEX uq_connected_app_grants_app_user ON connected_app_grants (app_id, user_id);

CREATE UNIQUE INDEX uq_location_ha_sensors_location_entity ON location_ha_sensors (location_id, entity_id);

CREATE UNIQUE INDEX uq_oidc_link_provider_sub ON user_oidc_links (provider_id, provider_user_id);

CREATE UNIQUE INDEX uq_oidc_link_user_provider ON user_oidc_links (user_id, provider_id);

CREATE UNIQUE INDEX uq_print_batches_external ON print_batches (external_source, external_ref);

CREATE UNIQUE INDEX uq_printer_maintenance_printer_type ON printer_maintenance (printer_id, maintenance_type_id);

CREATE UNIQUE INDEX uq_wallet_transactions_archive ON wallet_transactions (transaction_type, print_archive_id) WHERE print_run_id IS NULL;

CREATE UNIQUE INDEX uq_wallet_transactions_print_run ON wallet_transactions (transaction_type, print_run_id);

CREATE TRIGGER archive_fts_delete AFTER DELETE ON print_archives BEGIN
                    INSERT INTO archive_fts(archive_fts, rowid, print_name, filename, tags, notes, designer, filament_type)
                    VALUES ('delete', old.id, old.print_name, old.filename, old.tags, old.notes, old.designer, old.filament_type);
                END;

CREATE TRIGGER archive_fts_insert AFTER INSERT ON print_archives BEGIN
                    INSERT INTO archive_fts(rowid, print_name, filename, tags, notes, designer, filament_type)
                    VALUES (new.id, new.print_name, new.filename, new.tags, new.notes, new.designer, new.filament_type);
                END;

CREATE TRIGGER archive_fts_update AFTER UPDATE ON print_archives BEGIN
                    INSERT INTO archive_fts(archive_fts, rowid, print_name, filename, tags, notes, designer, filament_type)
                    VALUES ('delete', old.id, old.print_name, old.filename, old.tags, old.notes, old.designer, old.filament_type);
                    INSERT INTO archive_fts(rowid, print_name, filename, tags, notes, designer, filament_type)
                    VALUES (new.id, new.print_name, new.filename, new.tags, new.notes, new.designer, new.filament_type);
                END;

CREATE TRIGGER trg_filament_sku_settings_updated_at
               AFTER UPDATE ON filament_sku_settings FOR EACH ROW
               BEGIN
                 UPDATE filament_sku_settings SET updated_at = CURRENT_TIMESTAMP WHERE id = OLD.id;
               END;

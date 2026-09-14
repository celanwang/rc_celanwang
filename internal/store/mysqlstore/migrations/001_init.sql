CREATE TABLE IF NOT EXISTS schema_migration (
    version VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    applied_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS notification (
    id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    caller_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    idempotency_key VARCHAR(200) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    request_hash BINARY(32) NOT NULL,
    target_id VARCHAR(128) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
    request_url VARCHAR(2048) NOT NULL,
    request_method VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_headers JSON NOT NULL,
    request_body MEDIUMBLOB NOT NULL,
    status ENUM('pending','processing','succeeded','failed') NOT NULL,
    run_no INT UNSIGNED NOT NULL DEFAULT 1,
    run_attempt_count INT UNSIGNED NOT NULL DEFAULT 0,
    last_attempt_no INT UNSIGNED NOT NULL DEFAULT 0,
    max_attempts INT UNSIGNED NOT NULL,
    next_attempt_at DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NOT NULL,
    lease_token CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    lease_until DATETIME(6) NULL,
    last_http_status SMALLINT UNSIGNED NULL,
    last_error_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    last_error VARCHAR(512) NOT NULL DEFAULT '',
    stop_reason VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    replay_history JSON NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) ON UPDATE CURRENT_TIMESTAMP(6),
    finished_at DATETIME(6) NULL,
    UNIQUE KEY uq_notification_idempotency (caller_id, idempotency_key),
    KEY ix_notification_target_due (target_id, status, next_attempt_at, id),
    KEY ix_notification_lease (status, lease_until, id),
    KEY ix_notification_caller_created (caller_id, created_at, id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS notification_attempt (
    id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT PRIMARY KEY,
    notification_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    run_no INT UNSIGNED NOT NULL,
    attempt_no INT UNSIGNED NOT NULL,
    started_at DATETIME(6) NOT NULL,
    finished_at DATETIME(6) NULL,
    result ENUM('in_progress','http_success','http_failure','transport_error','recovered_unknown') NOT NULL,
    http_status SMALLINT UNSIGNED NULL,
    error_code VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    phase VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    delivery_evidence VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    retry_after DATETIME(6) NULL,
    duration_ms BIGINT UNSIGNED NULL,
    response_summary VARCHAR(512) NOT NULL DEFAULT '',
    UNIQUE KEY uq_attempt_number (notification_id, attempt_no),
    CONSTRAINT fk_attempt_notification FOREIGN KEY (notification_id) REFERENCES notification(id) ON DELETE CASCADE
) ENGINE=InnoDB;

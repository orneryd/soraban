BEGIN;

CREATE TABLE schema_migrations (
    version bigint PRIMARY KEY,
    name text NOT NULL UNIQUE,
    applied_at timestamptz NOT NULL DEFAULT transaction_timestamp()
);

CREATE TABLE firms (
    id text PRIMARY KEY,
    CONSTRAINT firms_id_not_blank CHECK (btrim(id) <> '')
);

CREATE TABLE clients (
    firm_id text NOT NULL,
    id text NOT NULL,
    PRIMARY KEY (firm_id, id),
    CONSTRAINT clients_firm_fk FOREIGN KEY (firm_id) REFERENCES firms(id) ON DELETE RESTRICT,
    CONSTRAINT clients_id_not_blank CHECK (btrim(id) <> '')
);

CREATE TABLE datasets (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    firm_id text NOT NULL,
    tax_year integer NOT NULL,
    content_sha256 bytea NOT NULL,
    source_row_count bigint NOT NULL,
    imported_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT datasets_firm_year_uk UNIQUE (firm_id, tax_year),
    CONSTRAINT datasets_firm_hash_uk UNIQUE (firm_id, content_sha256),
    CONSTRAINT datasets_id_firm_uk UNIQUE (id, firm_id),
    CONSTRAINT datasets_firm_fk FOREIGN KEY (firm_id) REFERENCES firms(id) ON DELETE RESTRICT,
    CONSTRAINT datasets_hash_length_ck CHECK (octet_length(content_sha256) = 32),
    CONSTRAINT datasets_row_count_ck CHECK (source_row_count > 0),
    CONSTRAINT datasets_tax_year_ck CHECK (tax_year BETWEEN 1900 AND 9999)
);

CREATE TABLE payments (
    dataset_id bigint NOT NULL,
    source_row_number bigint NOT NULL,
    firm_id text NOT NULL,
    client_id text NOT NULL,
    vendor_name text NOT NULL,
    vendor_tin text,
    vendor_identity text NOT NULL,
    payment_date date NOT NULL,
    amount_cents bigint NOT NULL,
    payment_method text NOT NULL,
    backup_withholding_cents bigint NOT NULL,
    memo text NOT NULL,
    PRIMARY KEY (dataset_id, source_row_number),
    CONSTRAINT payments_dataset_firm_fk FOREIGN KEY (dataset_id, firm_id) REFERENCES datasets(id, firm_id) ON DELETE RESTRICT,
    CONSTRAINT payments_client_firm_fk FOREIGN KEY (firm_id, client_id) REFERENCES clients(firm_id, id) ON DELETE RESTRICT,
    CONSTRAINT payments_source_row_ck CHECK (source_row_number > 0),
    CONSTRAINT payments_vendor_name_ck CHECK (btrim(vendor_name) <> ''),
    CONSTRAINT payments_vendor_identity_ck CHECK (btrim(vendor_identity) <> ''),
    CONSTRAINT payments_method_ck CHECK (payment_method IN ('check', 'ach', 'wire', 'cash', 'credit_card', 'paypal')),
    CONSTRAINT payments_withholding_ck CHECK (backup_withholding_cents >= 0)
);

CREATE INDEX payments_determination_idx
    ON payments (dataset_id, client_id, vendor_identity)
    INCLUDE (amount_cents, payment_method, backup_withholding_cents, vendor_name);

CREATE VIEW payment_classification_nec_2025_v1 WITH (security_invoker = true) AS
SELECT p.*,
       p.payment_method IN ('check', 'ach', 'wire', 'cash') AS counted,
       CASE p.payment_method
           WHEN 'credit_card' THEN 'PAYMENT_PROCESSOR_REPORTED'::text
           WHEN 'paypal' THEN 'THIRD_PARTY_NETWORK_REPORTED'::text
           ELSE NULL::text
       END AS exclusion_reason
FROM payments p;

CREATE TABLE determinations (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    dataset_id bigint NOT NULL,
    firm_id text NOT NULL,
    ruleset_version text NOT NULL,
    completed_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT determinations_dataset_ruleset_uk UNIQUE (dataset_id, ruleset_version),
    CONSTRAINT determinations_id_firm_uk UNIQUE (id, firm_id),
    CONSTRAINT determinations_dataset_firm_fk FOREIGN KEY (dataset_id, firm_id) REFERENCES datasets(id, firm_id) ON DELETE RESTRICT,
    CONSTRAINT determinations_ruleset_ck CHECK (ruleset_version = 'nec-2025-v1')
);

CREATE TABLE filings (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    firm_id text NOT NULL,
    client_id text NOT NULL,
    determination_id bigint NOT NULL,
    tax_year integer NOT NULL,
    vendor_identity text NOT NULL,
    vendor_display_name text NOT NULL,
    recipient_tin text,
    filing_key bytea NOT NULL,
    reportable_cents bigint NOT NULL,
    withholding_cents bigint NOT NULL,
    state text NOT NULL,
    preflight_reason text,
    irs_record_id text,
    rejection_reason text,
    created_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    acknowledged_at timestamptz,
    CONSTRAINT filings_key_uk UNIQUE (filing_key),
    CONSTRAINT filings_business_identity_uk UNIQUE (firm_id, client_id, tax_year, vendor_identity),
    CONSTRAINT filings_id_firm_client_uk UNIQUE (id, firm_id, client_id),
    CONSTRAINT filings_determination_firm_fk FOREIGN KEY (determination_id, firm_id) REFERENCES determinations(id, firm_id) ON DELETE RESTRICT,
    CONSTRAINT filings_client_firm_fk FOREIGN KEY (firm_id, client_id) REFERENCES clients(firm_id, id) ON DELETE RESTRICT,
    CONSTRAINT filings_key_length_ck CHECK (octet_length(filing_key) = 32),
    CONSTRAINT filings_state_ck CHECK (state IN ('blocked_preflight', 'ready', 'batched', 'accepted', 'rejected')),
    CONSTRAINT filings_preflight_reason_ck CHECK (preflight_reason IS NULL OR preflight_reason IN ('TIN_MISSING', 'TIN_MALFORMED', 'TIN_INVALID', 'AMOUNT_INVALID')),
    CONSTRAINT filings_rejection_reason_ck CHECK (rejection_reason IS NULL OR rejection_reason IN ('TIN_MISSING', 'TIN_MALFORMED', 'TIN_INVALID', 'AMOUNT_INVALID')),
    CONSTRAINT filings_withholding_ck CHECK (withholding_cents >= 0),
    CONSTRAINT filings_result_shape_ck CHECK (
        (state = 'blocked_preflight' AND preflight_reason IS NOT NULL AND irs_record_id IS NULL AND rejection_reason IS NULL AND acknowledged_at IS NULL)
        OR (state IN ('ready', 'batched') AND preflight_reason IS NULL AND irs_record_id IS NULL AND rejection_reason IS NULL AND acknowledged_at IS NULL)
        OR (state = 'accepted' AND preflight_reason IS NULL AND irs_record_id IS NOT NULL AND rejection_reason IS NULL AND acknowledged_at IS NOT NULL)
        OR (state = 'rejected' AND preflight_reason IS NULL AND irs_record_id IS NULL AND rejection_reason IS NOT NULL AND acknowledged_at IS NOT NULL)
    )
);

CREATE INDEX filings_status_idx ON filings (firm_id, client_id, state);
CREATE INDEX filings_rejection_idx ON filings (rejection_reason) WHERE state = 'rejected';

CREATE TABLE submission_batches (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    firm_id text NOT NULL,
    client_id text NOT NULL,
    tax_year integer NOT NULL,
    utid text NOT NULL,
    canonical_xml bytea NOT NULL,
    payload_sha256 bytea NOT NULL,
    state text NOT NULL,
    receipt_id text,
    next_action_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    attempt_count integer NOT NULL DEFAULT 0,
    lease_owner text,
    lease_expires_at timestamptz,
    claim_token text,
    submitted_at timestamptz,
    acknowledged_at timestamptz,
    retry_exhausted_at timestamptz,
    last_error_code text,
    CONSTRAINT submission_batches_firm_utid_uk UNIQUE (firm_id, utid),
    CONSTRAINT submission_batches_receipt_uk UNIQUE (receipt_id),
    CONSTRAINT submission_batches_id_firm_client_uk UNIQUE (id, firm_id, client_id),
    CONSTRAINT submission_batches_id_firm_uk UNIQUE (id, firm_id),
    CONSTRAINT submission_batches_client_firm_fk FOREIGN KEY (firm_id, client_id) REFERENCES clients(firm_id, id) ON DELETE RESTRICT,
    CONSTRAINT submission_batches_hash_length_ck CHECK (octet_length(payload_sha256) = 32),
    CONSTRAINT submission_batches_payload_ck CHECK (octet_length(canonical_xml) > 0),
    CONSTRAINT submission_batches_state_ck CHECK (state IN ('planned', 'submitting', 'submit_unknown', 'submitted', 'awaiting_ack', 'acknowledged', 'invariant_failed')),
    CONSTRAINT submission_batches_lease_ck CHECK ((lease_owner IS NULL) = (lease_expires_at IS NULL) AND (lease_owner IS NULL) = (claim_token IS NULL)),
    CONSTRAINT submission_batches_attempt_ck CHECK (attempt_count >= 0),
    CONSTRAINT submission_batches_receipt_state_ck CHECK (receipt_id IS NULL OR state IN ('submitted', 'awaiting_ack', 'acknowledged', 'invariant_failed')),
    CONSTRAINT submission_batches_ack_state_ck CHECK ((state = 'acknowledged') = (acknowledged_at IS NOT NULL)),
    CONSTRAINT submission_batches_retry_exhausted_ck CHECK (retry_exhausted_at IS NULL OR state = 'invariant_failed')
);

CREATE INDEX submission_batches_due_idx
    ON submission_batches (next_action_at, lease_expires_at, id)
    WHERE state NOT IN ('acknowledged', 'invariant_failed');

CREATE TABLE batch_filings (
    batch_id bigint NOT NULL,
    filing_id bigint NOT NULL,
    firm_id text NOT NULL,
    client_id text NOT NULL,
    slot integer NOT NULL,
    PRIMARY KEY (batch_id, filing_id),
    CONSTRAINT batch_filings_filing_uk UNIQUE (filing_id),
    CONSTRAINT batch_filings_slot_uk UNIQUE (batch_id, slot),
    CONSTRAINT batch_filings_batch_fk FOREIGN KEY (batch_id, firm_id, client_id) REFERENCES submission_batches(id, firm_id, client_id) ON DELETE RESTRICT,
    CONSTRAINT batch_filings_filing_fk FOREIGN KEY (filing_id, firm_id, client_id) REFERENCES filings(id, firm_id, client_id) ON DELETE RESTRICT,
    CONSTRAINT batch_filings_slot_ck CHECK (slot BETWEEN 1 AND 100)
);

CREATE TABLE rate_gates (
    firm_id text PRIMARY KEY,
    next_call_at timestamptz NOT NULL DEFAULT '-infinity',
    crash_fence_until timestamptz NOT NULL DEFAULT '-infinity',
    updated_at timestamptz NOT NULL DEFAULT transaction_timestamp(),
    CONSTRAINT rate_gates_firm_fk FOREIGN KEY (firm_id) REFERENCES firms(id) ON DELETE RESTRICT
);

CREATE TABLE api_call_log (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    firm_id text NOT NULL,
    batch_id bigint,
    operation text NOT NULL,
    started_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    completed_at timestamptz,
    outcome text,
    http_status integer,
    error_code text,
    CONSTRAINT api_call_log_firm_fk FOREIGN KEY (firm_id) REFERENCES firms(id) ON DELETE RESTRICT,
    CONSTRAINT api_call_log_batch_firm_fk FOREIGN KEY (batch_id, firm_id) REFERENCES submission_batches(id, firm_id) ON DELETE RESTRICT,
    CONSTRAINT api_call_log_operation_ck CHECK (operation IN ('submit', 'status')),
    CONSTRAINT api_call_log_outcome_ck CHECK (outcome IS NULL OR outcome IN ('completed', 'ambiguous', 'retryable_error', 'terminal_error')),
    CONSTRAINT api_call_log_http_status_ck CHECK (http_status IS NULL OR http_status BETWEEN 100 AND 599),
    CONSTRAINT api_call_log_completion_ck CHECK ((completed_at IS NULL) = (outcome IS NULL))
);

CREATE INDEX api_call_log_firm_started_idx ON api_call_log (firm_id, started_at DESC);

CREATE FUNCTION reject_all_changes() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = TG_TABLE_NAME || ' rows are immutable';
END
$$;

CREATE FUNCTION protect_filing_immutability() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.firm_id, OLD.client_id, OLD.determination_id, OLD.tax_year, OLD.vendor_identity,
           OLD.vendor_display_name, OLD.recipient_tin, OLD.filing_key, OLD.reportable_cents,
           OLD.withholding_cents, OLD.created_at)
       IS DISTINCT FROM
       ROW(NEW.firm_id, NEW.client_id, NEW.determination_id, NEW.tax_year, NEW.vendor_identity,
           NEW.vendor_display_name, NEW.recipient_tin, NEW.filing_key, NEW.reportable_cents,
           NEW.withholding_cents, NEW.created_at) THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = 'filing identity and amounts are immutable';
    END IF;
    IF OLD.state IN ('accepted', 'rejected') AND OLD IS DISTINCT FROM NEW THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = 'filing result is immutable';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION protect_batch_immutability() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.firm_id, OLD.client_id, OLD.tax_year, OLD.utid, OLD.canonical_xml, OLD.payload_sha256)
       IS DISTINCT FROM ROW(NEW.firm_id, NEW.client_id, NEW.tax_year, NEW.utid, NEW.canonical_xml, NEW.payload_sha256) THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = 'batch identity and payload are immutable';
    END IF;
    IF OLD.state IN ('acknowledged', 'invariant_failed') AND OLD IS DISTINCT FROM NEW THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = 'terminal batch is immutable';
    END IF;
    RETURN NEW;
END
$$;

CREATE FUNCTION protect_call_log() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF ROW(OLD.firm_id, OLD.batch_id, OLD.operation, OLD.started_at)
       IS DISTINCT FROM ROW(NEW.firm_id, NEW.batch_id, NEW.operation, NEW.started_at)
       OR OLD.completed_at IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = '55000', MESSAGE = 'call intent and completion are immutable';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER datasets_immutable BEFORE UPDATE OR DELETE ON datasets FOR EACH ROW EXECUTE FUNCTION reject_all_changes();
CREATE TRIGGER payments_immutable BEFORE UPDATE OR DELETE ON payments FOR EACH ROW EXECUTE FUNCTION reject_all_changes();
CREATE TRIGGER determinations_immutable BEFORE UPDATE OR DELETE ON determinations FOR EACH ROW EXECUTE FUNCTION reject_all_changes();
CREATE TRIGGER batch_filings_immutable BEFORE UPDATE OR DELETE ON batch_filings FOR EACH ROW EXECUTE FUNCTION reject_all_changes();
CREATE TRIGGER filings_immutable BEFORE UPDATE ON filings FOR EACH ROW EXECUTE FUNCTION protect_filing_immutability();
CREATE TRIGGER filings_no_delete BEFORE DELETE ON filings FOR EACH ROW EXECUTE FUNCTION reject_all_changes();
CREATE TRIGGER submission_batches_immutable BEFORE UPDATE ON submission_batches FOR EACH ROW EXECUTE FUNCTION protect_batch_immutability();
CREATE TRIGGER submission_batches_no_delete BEFORE DELETE ON submission_batches FOR EACH ROW EXECUTE FUNCTION reject_all_changes();
CREATE TRIGGER api_call_log_immutable BEFORE UPDATE ON api_call_log FOR EACH ROW EXECUTE FUNCTION protect_call_log();
CREATE TRIGGER api_call_log_no_delete BEFORE DELETE ON api_call_log FOR EACH ROW EXECUTE FUNCTION reject_all_changes();

DO $rls$
DECLARE
    table_name text;
    firm_expression text;
BEGIN
    FOREACH table_name IN ARRAY ARRAY['firms', 'clients', 'datasets', 'payments', 'determinations', 'filings', 'submission_batches', 'batch_filings', 'rate_gates', 'api_call_log'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', table_name);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', table_name);
        firm_expression := CASE WHEN table_name = 'firms' THEN 'id' ELSE 'firm_id' END;
        EXECUTE format(
            'CREATE POLICY %I ON %I USING (%s = current_setting(''app.firm_id'', true)) WITH CHECK (%s = current_setting(''app.firm_id'', true))',
            table_name || '_firm_policy', table_name, firm_expression, firm_expression
        );
    END LOOP;
END
$rls$;

GRANT SELECT ON schema_migrations TO readiness_app;
GRANT SELECT, INSERT ON firms, clients, datasets, payments, determinations, batch_filings TO readiness_app;
GRANT SELECT, INSERT, UPDATE ON filings, submission_batches, rate_gates, api_call_log TO readiness_app;
GRANT SELECT ON payment_classification_nec_2025_v1 TO readiness_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO readiness_app;
DO $grant_temp$
BEGIN
    EXECUTE format('GRANT TEMPORARY ON DATABASE %I TO readiness_app', current_database());
END
$grant_temp$;

INSERT INTO schema_migrations (version, name) VALUES (1, 'initial');

COMMIT;
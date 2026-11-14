-- +goose Up

-- A finding whose resource fits none of the plan's addresses (adr/0040).
-- Rows from before this that end in Trivy's module hash are marked; the
-- next plan of each workspace records them under their addresses.
ALTER TABLE findings ADD COLUMN unmatched boolean NOT NULL DEFAULT false;
UPDATE findings SET unmatched = true WHERE resource ~ '_[0-9a-f]{32}$';

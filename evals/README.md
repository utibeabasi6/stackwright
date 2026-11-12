# Agent evals

Each folder in `tasks/` is one task for the agent. `make eval` runs every
task against the real model and writes a score to `results/`. It spends
money: read [adr/0032](../docs/adr/0032-grounding-and-evals.md) first.

```bash
make eval ARM=baseline
```

```bash
make eval ARM=grounded
```

`ARM=baseline` hides `provider_schema` and `module_docs`. `ARM=grounded`
offers them. `STACKWRIGHT_EVAL_TASKS` takes a regular expression that
selects tasks by folder name. `STACKWRIGHT_TEST_OPENROUTER_KEY` comes from
the repository's `.env` or from the environment. `STACKWRIGHT_EVAL_TRANSCRIPTS`
names a folder for each task's full transcript.

## A task

- `task.json` holds the prompt and the checks.
- Every other file is the repository that the agent starts from.
- `solution/` holds a reference solution. The agent never sees it.

```json
{
  "prompt": "Add an S3 bucket named acme-logs with versioning enabled.",
  "base": "aws",
  "expect": [
    {"type": "aws_s3_bucket", "count": 1, "values": {"bucket": "acme-logs"}},
    {"type": "aws_s3_bucket_versioning", "values": {"versioning_configuration.0.status": "Enabled"}}
  ],
  "absent": ["aws_s3_bucket_acl"]
}
```

- `base` adds the files of `base/<name>` first. `base/aws` holds a
  provider that plans offline with fake keys.
- Each `expect` entry needs instances of `type` whose planned values match
  `values`. `count` is the exact number of matches; with no count, one is
  enough. A value path is dotted, with numbers for list items.
- `absent` lists resource types that the plan must not create.

`TestEvalSolutions` plans each seed and each solution with a local `tofu`
or `terraform`, with no model. The solution must pass and the seed must
fail:

```bash
STACKWRIGHT_TEST_ENGINES=1 go test -run TestEvalSolutions ./internal/server/
```

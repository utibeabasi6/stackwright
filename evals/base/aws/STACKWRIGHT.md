This repository is planned offline with fake AWS keys.

- Do not change the provider "aws" block in provider.tf. You may add
  entries to its required_providers.
- Do not use data sources that call AWS, such as aws_caller_identity,
  aws_availability_zones or aws_vpc. Write IDs as literals.
  aws_iam_policy_document is fine: it makes no call.

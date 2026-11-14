include "root" {
  path = find_in_parent_folders("root.hcl")
}
terraform {
  source = "../../modules/thing"
}
dependency "vpc" {
  config_path = "../vpc"
  mock_outputs = {
    id = "mock-vpc"
  }
  mock_outputs_allowed_terraform_commands = ["plan", "validate"]
}
inputs = {
  name     = "app"
  upstream = dependency.vpc.outputs.id
}

include "root" {
  path = find_in_parent_folders("root.hcl")
}
terraform {
  source = "../..//modules/pet"
}
inputs = {
  name = "vpc"
}

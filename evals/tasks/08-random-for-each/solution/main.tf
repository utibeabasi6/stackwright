terraform {
  required_providers {
    random = { source = "hashicorp/random" }
  }
}

variable "environments" {
  type    = list(string)
  default = ["dev", "staging", "prod"]
}

resource "random_id" "suffix" {
  for_each    = toset(var.environments)
  byte_length = 4
}

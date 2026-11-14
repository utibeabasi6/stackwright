terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "3.7.2"
    }
  }
}
variable "name" {}
resource "random_pet" "this" {
  prefix = var.name
}
output "id" {
  value = random_pet.this.id
}

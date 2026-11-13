terraform {
  backend "s3" {
    bucket = "acme-state"
    key    = "infra.tfstate"
    region = "eu-west-1"
  }
}
resource "random_pet" "a" {}

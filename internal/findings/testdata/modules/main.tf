terraform {
  required_providers {
    aws = { source = "hashicorp/aws", version = "5.100.0" }
  }
}

provider "aws" {
  region                      = "eu-west-1"
  access_key                  = "fixture"
  secret_key                  = "fixture"
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  skip_region_validation      = true
}

module "web" {
  source = "./modules/sg"
  name   = "web"
}

module "app" {
  source = "./modules/sg"
  name   = "app"
}

module "many" {
  source = "./modules/sg"
  count  = 2
  name   = "many-${count.index}"
}

module "keyed" {
  source   = "./modules/sg"
  for_each = toset(["a", "b"])
  name     = "keyed-${each.key}"
}

resource "aws_security_group" "root" {
  name   = "root"
  vpc_id = "vpc-0a1b2c3d"
  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

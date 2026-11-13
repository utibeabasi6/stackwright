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

# The network workspace's ids.
variable "vpc_id" {
  default = "vpc-0a1b2c3d"
}

variable "public_subnets" {
  default = ["subnet-0pub0", "subnet-0pub1"]
}

variable "private_subnets" {
  default = ["subnet-0pri0", "subnet-0pri1"]
}

resource "aws_security_group" "web" {
  name   = "web"
  vpc_id = var.vpc_id
  ingress {
    description = "HTTPS"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_security_group" "app" {
  name   = "app"
  vpc_id = var.vpc_id
  ingress {
    description     = "From the load balancer"
    from_port       = 8080
    to_port         = 8080
    protocol        = "tcp"
    security_groups = [aws_security_group.web.id]
  }
}

resource "aws_security_group" "db" {
  name   = "db"
  vpc_id = var.vpc_id
}

resource "aws_vpc_security_group_ingress_rule" "db_from_app" {
  security_group_id            = aws_security_group.db.id
  referenced_security_group_id = aws_security_group.app.id
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
}

resource "aws_lb" "web" {
  name            = "web"
  subnets         = var.public_subnets
  security_groups = [aws_security_group.web.id]
}

resource "aws_launch_template" "app" {
  name_prefix            = "app-"
  image_id               = "ami-0123456789abcdef0"
  instance_type          = "t3.small"
  vpc_security_group_ids = [aws_security_group.app.id]
}

resource "aws_autoscaling_group" "app" {
  name                = "app"
  min_size            = 2
  max_size            = 4
  vpc_zone_identifier = var.private_subnets
  launch_template {
    id = aws_launch_template.app.id
  }
}

resource "aws_db_subnet_group" "main" {
  name       = "main"
  subnet_ids = var.private_subnets
}

resource "aws_db_instance" "main" {
  identifier                  = "main"
  engine                      = "postgres"
  instance_class              = "db.t4g.micro"
  allocated_storage           = 20
  username                    = "app"
  manage_master_user_password = true
  db_subnet_group_name        = aws_db_subnet_group.main.name
  vpc_security_group_ids      = [aws_security_group.db.id]
  skip_final_snapshot         = true
}

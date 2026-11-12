resource "aws_ecs_cluster" "apps" {
  name = "apps"
  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

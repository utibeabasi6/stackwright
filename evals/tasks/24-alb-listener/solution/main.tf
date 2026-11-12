resource "aws_lb" "web" {
  name               = "web"
  load_balancer_type = "application"
  subnets            = ["subnet-0aaa1111", "subnet-0bbb2222"]
}

resource "aws_lb_target_group" "web" {
  name     = "web"
  port     = 8080
  protocol = "HTTP"
  vpc_id   = "vpc-0abc1234"
  health_check {
    path = "/healthz"
  }
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.web.arn
  port              = 80
  protocol          = "HTTP"
  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.web.arn
  }
}

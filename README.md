# go-balance
Simple attempt at load balancer

## Running the demo

Run:

`go run .`

The app now starts:
- the load balancer on `:8080`
- 5 demo backend servers on `localhost:5001` to `localhost:5005`

Test:

`curl -i http://localhost:8080/`

Stop with `Ctrl+C`. The load balancer and all demo backends shut down gracefully.

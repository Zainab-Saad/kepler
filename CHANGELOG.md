in kepler 0.7 release
- switch to libbpf as default ebpf provider
- base image update decouple GPU driver from kepler image itself
- use kprobe instead of tracepoint for ebpf to obtain context switch information
- add task clock event to ebpf and use it to calculate cpu usage for each process. The event is also exported to prometheus
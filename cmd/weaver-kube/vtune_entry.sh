#!/bin/sh

# RUN_CMD Comes from ENV in dockerfile

working_dir=/weaver
# trailing / means add a unique suffix to the end of each analysis result
result_dir=/tmp/vtune_results/analyze_pod/ 
profile_time=unlimited

cd /vtune/latest
source vtune-vars.sh

vtune -collect hotspots               \
  -knob sampling-mode=hw              \
  -knob enable-stack-collection=true  \
  --app-working-dir=$working_dir      \
  -result-dir=$result_dir             \
  --duration $profile_time            \
  -- $RUN_CMD $*

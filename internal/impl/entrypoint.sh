#!/bin/sh

working_dir=/weaver
# RUN_CMD=$1
result_dir=/tmp/vtune_results/analyze_pod/
profile_time= 30

cd /vtune/latest
source vtune-vars.sh

vtune -collect hotspots -knob sampling-mode=hw -knob enable-stack-collection=true --app-working-dir=$working_dir -result-dir=$result_dir --duration $profile_time -- $RUN_CMD $*

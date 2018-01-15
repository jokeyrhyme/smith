#!/usr/bin/env bash

export KUBE_PATCH_CONVERSION_DETECTOR=true
export KUBE_CACHE_MUTATION_DETECTOR=true

binary="$1"
shift
"$binary" $@

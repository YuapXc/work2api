<script setup lang="ts">
import MediaWorkspace from '../../src/components/MediaWorkspace.vue'
import { api } from '../api'
const loadKeys=async()=>(await api.keys()).keys
const readKey=async(id:number)=>(await api.key(id)).key || ''
</script>
<template><MediaWorkspace :load-keys="loadKeys" :read-key="readKey"/></template>

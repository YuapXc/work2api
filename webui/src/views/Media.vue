<script setup lang="ts">
import MediaWorkspace from '@/components/MediaWorkspace.vue'
import { api } from '@/api/client'
const loadKeys=async()=>(await api.apps()).apps
const readKey=async(id:number)=>(await api.appKey(id)).key || ''
</script>
<template><MediaWorkspace :load-keys="loadKeys" :read-key="readKey"/></template>

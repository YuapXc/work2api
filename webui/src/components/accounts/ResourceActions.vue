<script setup lang="ts">
import type { ResourceSummary } from '@/types'
import WButton from '@/components/ui/WButton.vue'
import WToggle from '@/components/ui/WToggle.vue'
defineProps<{ resource?: ResourceSummary; enabled: boolean; busy?: boolean }>()
defineEmits<{ rename: []; remove: []; enabled: [] }>()
</script>

<template>
  <div class="flex items-center justify-end gap-1.5">
    <span v-if="!resource" class="text-micro text-faint">只读</span>
    <WButton v-if="resource?.actions.rename" size="sm" variant="subtle" :disabled="busy || !resource.actions.rename.enabled" :title="resource.actions.rename.reason" @click="$emit('rename')">{{ resource.actions.rename.label }}</WButton>
    <WToggle v-if="resource?.actions.enabled" :model-value="enabled" :disabled="busy || !resource.actions.enabled.enabled" aria-label="启用账号" @update:model-value="$emit('enabled')" />
    <WButton v-if="resource?.actions.delete" size="sm" variant="danger" :disabled="busy || !resource.actions.delete.enabled" :title="resource.actions.delete.reason" @click="$emit('remove')">{{ resource.actions.delete.label }}</WButton>
  </div>
</template>

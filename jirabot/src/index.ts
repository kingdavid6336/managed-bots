import util from 'util'
import Bot from './bot'
import {init} from './context'
import * as BotConfig from './bot-config'
import startHTTPServer from './http-server'
import startBackgroundTasks from './background-tasks'
import logger from './logger'

process.on('unhandledRejection', (reason, promise) => {
  logger.fatal({
    msg: 'unhandled promise rejection',
    reason: reason?.toString(),
    promise: util.format(promise),
  })
  process.exit(1)
})

const botConfig = BotConfig.parse(process.env.JIRABOT_CONFIG || '')
if (!botConfig) {
  logger.fatal('invalid bot-config')
  process.exit(1)
} else {
  init(botConfig).then(context => {
    startHTTPServer(context)
    startBackgroundTasks(context)
    Bot(context)
  })
}

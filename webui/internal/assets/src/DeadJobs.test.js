import './TestSetup';
import expect from 'expect';
import DeadJobs from './DeadJobs';
import React from 'react';
import { mount } from 'enzyme';

describe('DeadJobs', () => {
  let originalConfirm;

  beforeEach(() => {
    originalConfirm = window.confirm;
    window.confirm = function() { return true; }; // Default to confirming actions
  });

  afterEach(() => {
    window.confirm = originalConfirm;
  });

  it('shows dead jobs', () => {
    let deadJobs = mount(<DeadJobs />);

    expect(deadJobs.state().selected.length).toEqual(0);
    expect(deadJobs.state().jobs.length).toEqual(0);

    deadJobs.setState({
      count: 2,
      jobs: [
        {id: 1, name: 'test', args: {}, t: 1467760821, err: 'err1'},
        {id: 2, name: 'test2', args: {}, t: 1467760822, err: 'err2'}
      ]
    });

    expect(deadJobs.state().selected.length).toEqual(0);
    expect(deadJobs.state().jobs.length).toEqual(2);

    let checkbox = deadJobs.find('input');
    expect(checkbox.length).toEqual(3);
    expect(checkbox.at(0).props().checked).toEqual(false);
    expect(checkbox.at(1).props().checked).toEqual(false);
    expect(checkbox.at(2).props().checked).toEqual(false);

    checkbox.at(0).simulate('change');
    checkbox = deadJobs.find('input');
    expect(checkbox.length).toEqual(3);
    expect(checkbox.at(0).props().checked).toEqual(true);
    expect(checkbox.at(1).props().checked).toEqual(true);
    expect(checkbox.at(2).props().checked).toEqual(true);

    checkbox.at(1).simulate('change');
    checkbox = deadJobs.find('input');
    expect(checkbox.length).toEqual(3);
    expect(checkbox.at(0).props().checked).toEqual(true);
    expect(checkbox.at(1).props().checked).toEqual(false);
    expect(checkbox.at(2).props().checked).toEqual(true);

    checkbox.at(1).simulate('change');
    checkbox = deadJobs.find('input');
    expect(checkbox.length).toEqual(3);
    expect(checkbox.at(0).props().checked).toEqual(true);
    expect(checkbox.at(1).props().checked).toEqual(true);
    expect(checkbox.at(2).props().checked).toEqual(true);

    let button = deadJobs.find('button');
    expect(button.length).toEqual(4);
    button.at(0).simulate('click');
    button.at(1).simulate('click');
    button.at(2).simulate('click');
    button.at(3).simulate('click');

    checkbox.at(0).simulate('change');

    checkbox = deadJobs.find('input');
    expect(checkbox.length).toEqual(3);
    expect(checkbox.at(0).props().checked).toEqual(false);
    expect(checkbox.at(1).props().checked).toEqual(false);
    expect(checkbox.at(2).props().checked).toEqual(false);
  });

  it('handles confirmation dialogs', () => {
    let deadJobs = mount(<DeadJobs />);
    let confirmCalls = [];

    // Override window.confirm to track calls
    window.confirm = function(message) {
      confirmCalls.push(message);
      return true;
    };

    // Test delete selected with confirmation
    deadJobs.find('button').at(0).simulate('click');
    expect(confirmCalls[0]).toBe('Are you sure you want to delete the selected jobs?');

    // Test retry selected with confirmation
    deadJobs.find('button').at(1).simulate('click');
    expect(confirmCalls[1]).toBe('Are you sure you want to retry the selected jobs?');

    // Test delete all with confirmation
    deadJobs.find('button').at(2).simulate('click');
    expect(confirmCalls[2]).toBe('Are you sure you want to delete all jobs?');

    // Test retry all with confirmation
    deadJobs.find('button').at(3).simulate('click');
    expect(confirmCalls[3]).toBe('Are you sure you want to retry all jobs?');

    // Test cancellation
    window.confirm = function() { return false; };
    deadJobs.find('button').at(0).simulate('click');
    // The action should not be performed when cancelled
  });

  it('has pages', () => {
    let deadJobs = mount(<DeadJobs />);

    let genJob = (n) => {
      let job = [];
      for (let i = 1; i <= n; i++) {
        job.push({
          id: i,
          name: 'test',
          args: {},
          t: 1467760821,
          err: 'err',
        });
      }
      return job;
    };
    deadJobs.setState({
      count: 21,
      jobs: genJob(21)
    });

    expect(deadJobs.state().jobs.length).toEqual(21);
    expect(deadJobs.state().page).toEqual(1);

    let pageList = deadJobs.find('PageList');
    expect(pageList.length).toEqual(1);

    pageList.at(0).props().jumpTo(2)();
    expect(deadJobs.state().page).toEqual(2);
  });
});
